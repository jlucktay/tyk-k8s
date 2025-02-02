package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/mitchellh/go-homedir"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"go.jlucktay.dev/tyk-k8s/tyk"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "tyk-k8s",
	Short: "Tyk controller utility for kubernetes",
	Long: `Provides a sidecar injector web service and an ingress
controller service, start the controller with:

	tyk-k8s start

or you can start both by chaining the arguments`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.tyk-k8s-controller.yaml)")
}

// initConfig reads in config file and ENV variables if set.
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag.
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory.
		home, err := homedir.Dir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		// Search config in home directory with name ".tyk-k8s-controller" (without extension).
		viper.AddConfigPath(".")
		viper.AddConfigPath(home)
		viper.AddConfigPath("/etc/tyk-k8s")
		viper.SetConfigName("tyk-k8s")
	}

	viper.SetEnvPrefix("tk8s")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv() // read in environment variables that match

	// If a config file is found, read it in.
	if err := viper.ReadInConfig(); err != nil {
		log.Fatal(err)
	}

	// workaround because viper does not treat env vars the same as other config
	for _, key := range viper.AllKeys() {
		val := viper.Get(key)
		viper.Set(key, val)
	}

	log.Infof("Using config file: %v", viper.ConfigFileUsed())
	tyk.Init(nil)
}
