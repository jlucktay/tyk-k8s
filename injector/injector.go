package injector

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/ghodss/yaml"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/intstr"

	"go.jlucktay.dev/tyk-k8s/ca"
	"go.jlucktay.dev/tyk-k8s/logger"
	"go.jlucktay.dev/tyk-k8s/tyk"
)

var log = logger.GetLogger("injector")

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

var ignoredNamespaces = []string{
	metav1.NamespaceSystem,
	metav1.NamespacePublic,
}

const (
	// Injector toggle and listen path to generate
	AdmissionWebhookAnnotationInjectKey = "injector.tyk.io/inject"
	admissionWebhookAnnotationRouteKey  = "injector.tyk.io/route"

	// Internally used by mesh to track API IDs and state
	AdmissionWebhookAnnotationStatusKey           = "injector.tyk.io/status"
	AdmissionWebhookAnnotationInboundServiceIDKey = "injector.tyk.io/inbound-service-id"
	AdmissionWebhookAnnotationMeshServiceIDKey    = "injector.tyk.io/mesh-service-id"

	// Advanced mesh keys for group access policies TBC
	AdmissionWebhookAnnotationGroupKey            = "injector.tyk.io/group"
	AdmissionWebhookAnnotationAllowedCallerGroups = "injector.tyk.io/caller-access-groups"

	meshTag = "mesh"
)

type WebhookServer struct {
	SidecarConfig *Config
	CAConfig      *ca.Config
	CAClient      ca.CertClient
}

type Config struct {
	Containers        []corev1.Container `yaml:"containers"`
	InitContainers    []corev1.Container `yaml:"initContainers"`
	CreateRoutes      bool               `yaml:"createRoutes"`
	EnableMeshTLS     bool               `yaml:"enableMeshTLS"`
	MeshCertificateID string             `yaml:"meshCertificateID"`
}

type namedThing struct {
	metav1.TypeMeta `json:",inline"`
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/api-conventions.md#metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty" protobuf:"bytes,1,opt,name=metadata"`
}

type patchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
}

func loadConfig(configFile string) (*Config, error) {
	data, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	log.Infof("New configuration: sha256sum %x", sha256.Sum256(data))

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Check whether the target resoured need to be mutated
func mutationRequired(ignoredList []string, metadata *metav1.ObjectMeta) bool {
	// skip special kubernete system namespaces
	for _, namespace := range ignoredList {
		if metadata.Namespace == namespace {
			log.Infof("Skip mutation for %v, special namespace:%v", metadata.Name, metadata.Namespace)
			return false
		}
	}

	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	status := annotations[AdmissionWebhookAnnotationStatusKey]

	// determine whether to perform mutation based on annotation for the target resource
	var required bool
	if strings.ToLower(status) == "injected" {
		required = false
	} else {
		switch strings.ToLower(annotations[AdmissionWebhookAnnotationInjectKey]) {
		default:
			required = false
		case "y", "yes", "true", "on":
			required = true
		}
	}

	log.Infof("Mutation policy for %v/%v: status: %q required:%v", metadata.Namespace, metadata.Name, status, required)
	return required
}

func addContainer(pod *corev1.Pod, added []corev1.Container) *corev1.PodSpec {
	spec := &pod.Spec
	if len(spec.Containers) == 0 {
		spec.Containers = []corev1.Container{}
	}

	if len(spec.HostAliases) == 0 {
		spec.HostAliases = []corev1.HostAlias{}
	}

	spec.HostAliases = append(spec.HostAliases, corev1.HostAlias{
		IP:        "127.0.0.1",
		Hostnames: []string{"mesh", "mesh.local"},
	})

	for idx := range added {
		spec.Containers = append(spec.Containers, added[idx])
	}

	return spec
}

func addInitContainer(spec *corev1.PodSpec, added []corev1.Container) *corev1.PodSpec {
	if len(spec.InitContainers) == 0 {
		spec.InitContainers = []corev1.Container{}
	}

	for idx := range added {
		spec.InitContainers = append(spec.InitContainers, added[idx])
	}

	return spec
}

func updateAnnotation(target, added map[string]string) (patch []patchOperation) {
	if target == nil {
		target = map[string]string{}
	}

	patch = append(patch, patchOperation{
		Op:    "add",
		Path:  "/metadata/annotations",
		Value: added,
	})

	return patch
}

func mutateService(svc *corev1.Service, basePath string, sidecarConfig *Config) (patch []patchOperation) {
	var sidecarPort int32 = 8080

	sidecarSvcPort := &corev1.ServicePort{
		Name: "tyk-sidecar",
		Port: sidecarPort,
		TargetPort: intstr.IntOrString{
			IntVal: sidecarPort,
		},
	}

	opp := "replace"
	path := "/spec/ports/0"
	if len(svc.Spec.Ports) > 1 {
		opp = "add"
		path = "/spec/ports"
	}

	patch = append(patch, patchOperation{
		Op:    opp,
		Path:  path,
		Value: sidecarSvcPort,
	})

	return patch
}

func addVolume(spec *corev1.PodSpec, sidecarConfig *Config) *corev1.PodSpec {
	if !sidecarConfig.EnableMeshTLS {
		return spec
	}

	// Add the overall shared volume
	volume := corev1.Volume{
		Name: volName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: volName,
				},
			},
		},
	}

	sslCerts := corev1.Volume{
		Name: certVolumenName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}

	if spec.Volumes == nil {
		spec.Volumes = []corev1.Volume{}
	}
	spec.Volumes = append(spec.Volumes, volume)
	spec.Volumes = append(spec.Volumes, sslCerts)

	return spec
}

var (
	volName         = "ca-pem"
	certVolumenName = "ssl-certs"
)

func injectCAVolume(spec *corev1.PodSpec, sidecarConfig *Config) *corev1.PodSpec {
	if !sidecarConfig.EnableMeshTLS {
		return spec
	}

	// path := fmt.Sprintf("/spec/containers")
	for idx := range spec.Containers {
		// Mount SSL certs from the init container
		volumeMount := corev1.VolumeMount{
			Name:      certVolumenName,
			MountPath: "/etc/ssl/certs",
		}

		// If there is no section, add
		if spec.Containers[idx].VolumeMounts == nil {
			log.Info("adding new mount section")
			spec.Containers[idx].VolumeMounts = []corev1.VolumeMount{}
		}
		spec.Containers[idx].VolumeMounts = append(spec.Containers[idx].VolumeMounts, volumeMount)
	}

	return spec
}

// add tags to the gateway container
const tagVarName = "TYK_GW_DBAPPCONFOPTIONS_TAGS"

// TODO: For some reason this starts appending the same (or different) tags after multiple deployments
func preProcessContainerTpl(pod *corev1.Pod, containers []corev1.Container) []corev1.Container {
	sName, ok := pod.Labels["app"]
	if !ok {
		sName = pod.GenerateName + "please-set-app-label"
	}

	tags := fmt.Sprintf("mesh,%s", sName)
	tagEnv := corev1.EnvVar{Name: tagVarName, Value: tags}
	for i, cnt := range containers {
		if strings.ToLower(cnt.Name) == "tyk-mesh" {
			for ei, envVal := range containers[i].Env {
				if envVal.Name == tagVarName {
					// update the existing variable
					containers[i].Env[ei] = tagEnv
					return containers
				}
			}

			// no exiting var found, create
			containers[i].Env = append(cnt.Env, corev1.EnvVar{Name: tagVarName, Value: tags})
			break
		}
	}

	return containers
}

// create mutation patch for resoures
func createPatch(pod *corev1.Pod, svc *corev1.Service, sidecarConfig *Config, annotations map[string]string) ([]byte, error) {
	var patch []patchOperation

	if svc != nil {
		patch = append(patch, mutateService(svc, "/spec/ports", sidecarConfig)...)
		return json.Marshal(patch)
	}

	spec := addContainer(pod, preProcessContainerTpl(pod, sidecarConfig.Containers))
	spec = addInitContainer(spec, sidecarConfig.InitContainers)
	spec = addVolume(spec, sidecarConfig)
	spec = injectCAVolume(spec, sidecarConfig)

	patch = append(patch, patchOperation{
		Op:    "replace",
		Path:  "/spec",
		Value: spec,
	})

	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

	return json.Marshal(patch)
}

func checkAndGetTemplate(pd *corev1.Pod, isMesh bool) string {
	for k, v := range pd.Annotations {
		if k == tyk.TemplateNameKey {
			return v
		}
	}

	if isMesh {
		return tyk.DefaultMeshTemplate
	}

	return tyk.DefaultInboundTemplate
}

// create service routes
func createServiceRoutes(pod *corev1.Pod, annotations map[string]string, namespace string, tls bool) (map[string]string, error) {
	_, idExists := annotations[AdmissionWebhookAnnotationInboundServiceIDKey]
	if idExists {
		return annotations, nil
	}

	sName, ok := pod.Labels["app"]
	if !ok {
		return annotations, errors.New("app label is required")
	}

	ns := namespace
	if ns == "" {
		ns = "default"
	}

	hName := fmt.Sprintf("%s.%s", sName, ns)
	slugID := sName + "-inbound"
	// inbound listener
	opts := &tyk.APIDefOptions{
		Slug:         slugID,
		Target:       "http://localhost:6767",
		ListenPath:   "/",
		TemplateName: checkAndGetTemplate(pod, false),
		Hostname:     hName,
		Name:         slugID,
		Tags:         []string{sName},
		Annotations:  annotations,
	}

	ibID := ""
	inboundDef, doNotSkip := tyk.GetBySlug(opts.Slug)
	if doNotSkip != nil {
		// error means this service hasn't been created yet
		inboundID, err := tyk.CreateService(opts)
		if err != nil {
			return annotations, fmt.Errorf("failed to create inbound service %v: %v", slugID, err.Error())
		}

		ibID = inboundID
	} else {
		ibID = inboundDef.Id.Hex()
	}

	annotations[AdmissionWebhookAnnotationInboundServiceIDKey] = ibID

	// mesh route points to the *service* so we can enable load balancing
	var pt int32
	pt = 8080

	tr := "http"
	if tls {
		tr = "https"
	}
	tgt := fmt.Sprintf("%s://%s:%d", tr, hName, pt)
	listenPath := sName
	for k, v := range pod.Annotations {
		if k == admissionWebhookAnnotationRouteKey {
			listenPath = v
		}
	}

	meshID := ""
	meshSlugID := sName + "-mesh"
	// meshHostName := fmt.Sprintf("%s.mesh", sName)
	meshOpts := &tyk.APIDefOptions{
		Slug:         meshSlugID,
		Target:       tgt,
		ListenPath:   listenPath,
		TemplateName: checkAndGetTemplate(pod, true),
		Hostname:     "mesh",
		Name:         meshSlugID,
		Tags:         []string{meshTag},
	}

	meshDef, doNotSkipMesh := tyk.GetBySlug(meshOpts.Slug)
	if doNotSkipMesh != nil {
		// error means this service hasn't been created yet
		mId, err := tyk.CreateService(meshOpts)
		if err != nil {
			return annotations, fmt.Errorf("failed to create mesh service %v: %v", meshSlugID, err.Error())
		}
		meshID = mId
	} else {
		meshID = meshDef.Id.Hex()
	}

	annotations[AdmissionWebhookAnnotationMeshServiceIDKey] = meshID

	return annotations, nil
}

func (whsvr *WebhookServer) generateStoreAndRegisterCertForAPIDef(sid, byoCert string) error {
	// Allow us to just manually set a cert ID
	certID := byoCert
	if byoCert == "" {
		certID = ""
		serverCert, err := whsvr.generateServerCert(sid)
		if err != nil {
			return fmt.Errorf("can't generate certificate: %v", err)
		}
		log.Info("MeshTLS: generated server certificate")

		certID, err = tyk.CreateCertificate(serverCert.Bundle.Bundled, serverCert.Bundle.PrivateKey)
		if err != nil {
			return fmt.Errorf("failed to upload certificate to tyk secure store: %v", err)
		}
		log.Info("MeshTLS: uploaded certificate to tyk secure store")
		serverCert.Bundle.Fingerprint = certID

		log.Info("MeshTLS: updated API definition to use new cert fingerprint")
		_, err = whsvr.CAClient.StoreCert(serverCert)
		if err != nil {
			return fmt.Errorf("failed to store certificate reference in controller store: %v", err)
		}
		log.Info("MeshTLS: stored new certificate in mongo")
	}

	aDef, err := tyk.GetByObjectID(sid)
	if err != nil {
		return fmt.Errorf("failed to retrieve API definition: %v", err)
	}

	if len(aDef.Certificates) == 0 {
		aDef.Certificates = make([]string, 0)
	}

	aDef.Certificates = append(aDef.Certificates, certID)
	err = tyk.UpdateAPI(&aDef.APIDefinition)
	if err != nil {
		return fmt.Errorf("failed to store updated API Definition (%v): %v", aDef.Id.Hex(), err)
	}

	return nil
}

func (whsvr *WebhookServer) handleMeshTLS(ann map[string]string) error {
	if !whsvr.SidecarConfig.EnableMeshTLS {
		log.Info("mesh TLS disabled, skipping check")
		// no TLS needed, skip
		return nil
	}

	// Validate and get required configuration

	// For mTLS we will need the mesh API ID
	//meshID, ok := ann[AdmissionWebhookAnnotationMeshServiceIDKey]
	//if !ok {
	//	return fmt.Errorf("can't generate server cert without a mesh ID")
	//}

	ingressID, ok := ann[AdmissionWebhookAnnotationInboundServiceIDKey]
	if !ok {
		return fmt.Errorf("can't generate server cert without an inbound API ID")
	}

	// Handle inbound ID first as that's a straight TLS cert
	log.Info("MeshTLS: starting last-mile TLS generation")
	err := whsvr.generateStoreAndRegisterCertForAPIDef(ingressID, "")
	if err != nil {
		return err
	}

	// we add a cert for https://mesh so that we can guarantee TLS all the way through
	meshID, ok := ann[AdmissionWebhookAnnotationMeshServiceIDKey]
	if !ok {
		return fmt.Errorf("can't generate server cert without an mesh API ID")
	}

	err = whsvr.generateStoreAndRegisterCertForAPIDef(meshID, whsvr.SidecarConfig.MeshCertificateID)
	if err != nil {
		return err
	}

	return nil
}

func (whsvr *WebhookServer) generateServerCert(id string) (*ca.CertModel, error) {
	apidef, err := tyk.GetByObjectID(id)
	if err != nil {
		return nil, err
	}

	hostname := apidef.Domain
	if hostname == "" {
		return nil, fmt.Errorf("domain cannot be emtpy")
	}

	bdl, err := whsvr.CAClient.GenerateCert(hostname)
	if err != nil {
		return nil, err
	}

	return ca.NewCertModel(bdl), nil
}

func (whsvr *WebhookServer) processPodMutations(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Errorf("Could not unmarshal raw object: %v", err)
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	log.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, pod.Name, req.UID, req.Operation, req.UserInfo)

	// determine whether to perform mutation
	if !mutationRequired(ignoredNamespaces, &pod.ObjectMeta) {
		log.Infof("Skipping mutation for %s/%s due to policy check", pod.Namespace, pod.Name)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	annotations := pod.Annotations
	annotations[AdmissionWebhookAnnotationStatusKey] = "injected"
	delete(annotations, AdmissionWebhookAnnotationInjectKey)

	// We create the service routes first, because we need the IDs
	if whsvr.SidecarConfig.CreateRoutes {
		var err error
		annotations, err = createServiceRoutes(&pod, annotations, ar.Request.Namespace, whsvr.SidecarConfig.EnableMeshTLS)
		if err != nil {
			return &v1beta1.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		}
	}

	// === TLS Specific operations ===
	if err := whsvr.handleMeshTLS(annotations); err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}
	// === End TLS ====

	// Create the patch
	patchBytes, err := createPatch(&pod, nil, whsvr.SidecarConfig, annotations)
	if err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	log.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func (whsvr *WebhookServer) processServiceMutations(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request
	var service corev1.Service
	if err := json.Unmarshal(req.Object.Raw, &service); err != nil {
		log.Errorf("Could not unmarshal raw object: %v", err)
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	log.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, service.Name, req.UID, req.Operation, req.UserInfo)

	// determine whether to perform mutation
	if !mutationRequired(ignoredNamespaces, &service.ObjectMeta) {
		log.Infof("SERVICE: Skipping mutation for %s/%s due to policy check", service.Namespace, service.Name)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	annotations := service.Annotations
	annotations[AdmissionWebhookAnnotationStatusKey] = "injected"
	delete(annotations, AdmissionWebhookAnnotationInjectKey)

	// Create the patch
	patchBytes, err := createPatch(nil, &service, whsvr.SidecarConfig, annotations)
	if err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	log.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

// main mutation process
func (whsvr *WebhookServer) mutate(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	req := ar.Request

	log.Info("object is: ", req.Kind)
	switch strings.ToLower(req.Kind.Kind) {
	case "pod":
		return whsvr.processPodMutations(ar)
	case "service":
		return whsvr.processServiceMutations(ar)
	default:
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: "type not supported",
			},
		}
	}
}

// Serve method for webhook server
func (whsvr *WebhookServer) Serve(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		log.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}

	var admissionResponse *v1beta1.AdmissionResponse
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		log.Errorf("can't decode body: %v", err)
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = whsvr.mutate(&ar)
	}

	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		log.Errorf("can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	log.Infof("ready to write reponse ...")
	if _, err := w.Write(resp); err != nil {
		log.Errorf("can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}
}
