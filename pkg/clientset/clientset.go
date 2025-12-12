package clientset

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// GetClient returns a clientset for the given kubeconfig
// If kubeconfig is empty, it will use the default kubeconfig
// preferring the environment variable.
func GetClient(kubeconfig string) (*rest.Config, *kubernetes.Clientset, error) {
	if kubeconfig != "" {
		return getClientset(kubeconfig)
	}

	// Use environment variable first
	if kubeconfig = os.Getenv("KUBECONFIG"); kubeconfig != "" {
		return getClientset(kubeconfig)
	}

	// fall back to the default kubeconfig
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	}
	
	return getClientset(kubeconfig)
}

func getClientset(kubeconfig string) (*rest.Config, *kubernetes.Clientset, error) {
	if kubeconfig == "" {
		return nil, nil, fmt.Errorf("kubeconfig is empty")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, nil, fmt.Errorf("can not create client-go configuration: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("can not create client-go client: %v", err)
	}
	return config, clientset, nil
}