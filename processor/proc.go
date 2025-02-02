package processor

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/tidwall/sjson"

	"go.jlucktay.dev/tyk-k8s/logger"
)

type ValueType string

const (
	ValueSetStringKey ValueType = "string.service.tyk.io/"
	ValueSetBoolKey   ValueType = "bool.service.tyk.io/"
	ValueSetNumKey    ValueType = "num.service.tyk.io/"
	ObjectSetKey      ValueType = "object.service.tyk.io/"
	ArraySetKey       ValueType = "array.service.tyk.io/"
)

var log = logger.GetLogger("processor")

func set(key, val, def string, t ValueType) (string, error) {
	pth := key[len(string(t)):]
	pth = strings.Replace(pth, "-", "_", -1)

	switch t {
	case ValueSetStringKey:
		log.Info("setting string value: ", pth)
		return sjson.Set(def, pth, val)
	case ValueSetBoolKey:
		log.Info("setting bool value: ", pth)
		b := false
		switch strings.ToLower(val) {
		case "true":
			b = true
		case "false":
			b = false
		default:
			return def, errors.New("unsupported bool value")
		}

		return sjson.Set(def, pth, b)
	case ValueSetNumKey:
		log.Info("setting num value: ", pth)
		d, err := strconv.Atoi(val)
		if err != nil {
			return def, err
		}

		return sjson.Set(def, pth, d)
	case ObjectSetKey:
		log.Info("setting object: ", pth)
		d := make(map[string]interface{}, 0)
		err := json.Unmarshal([]byte(val), &d)
		if err != nil {
			return def, err
		}

		return sjson.Set(def, pth, d)
	case ArraySetKey:
		log.Info("setting array: ", pth)
		d := make([]interface{}, 0)
		err := json.Unmarshal([]byte(val), &d)
		if err != nil {
			return def, err
		}

		return sjson.Set(def, pth, d)
	default:
		return def, errors.New("unsupported type")
	}
}

func Process(ann map[string]string, def string) (string, error) {
	var err error
	for k, v := range ann {
		if strings.HasPrefix(k, string(ValueSetStringKey)) {
			def, err = set(k, v, def, ValueSetStringKey)
			if err != nil {
				return def, err
			}
		}

		if strings.HasPrefix(k, string(ValueSetNumKey)) {
			def, err = set(k, v, def, ValueSetNumKey)
			if err != nil {
				return def, err
			}
		}

		if strings.HasPrefix(k, string(ValueSetBoolKey)) {
			def, err = set(k, v, def, ValueSetBoolKey)
			if err != nil {
				return def, err
			}
		}

		if strings.HasPrefix(k, string(ArraySetKey)) {
			def, err = set(k, v, def, ArraySetKey)
			if err != nil {
				return def, err
			}
		}

		if strings.HasPrefix(k, string(ObjectSetKey)) {
			def, err = set(k, v, def, ObjectSetKey)
			if err != nil {
				return def, err
			}
		}
	}

	return def, nil
}
