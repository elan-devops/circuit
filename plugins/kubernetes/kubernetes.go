package kubernetes

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/codeamp/circuit/plugins"
	log "github.com/codeamp/logger"
	"github.com/codeamp/transistor"
	"github.com/go-errors/errors"
	contour_client "github.com/heptio/contour/apis/generated/clientset/versioned"

	uuid "github.com/satori/go.uuid"
	v1 "k8s.io/api/core/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func init() {
	transistor.RegisterPlugin("kubernetes", func() transistor.Plugin {
		return &Kubernetes{
			K8sContourNamespacer: &ContourNamespace{},
			K8sNamespacer:        &KubernetesNamespace{},
			BatchV1Jobber:        &BatchV1Job{},
			CoreServicer:         &CoreService{},
			CoreSecreter:         &CoreSecret{},
			CoreDeploymenter:     &CoreDeployment{},
		}
	}, plugins.ReleaseExtension{}, plugins.ProjectExtension{})
}

func (x *Kubernetes) Description() string {
	return "Kubernetes"
}

func (x *Kubernetes) SampleConfig() string {
	return ` `
}

func (x *Kubernetes) Start(e chan transistor.Event) error {
	x.events = e
	log.Info("Started Kubernetes (k8s)")

	return nil
}

func (x *Kubernetes) Stop() {
	log.Info("Stopping Kubernetes (k8s)")
}

func (x *Kubernetes) Subscribe() []string {
	return []string{
		"project:kubernetes:deployment:create",
		"project:kubernetes:deployment:update",
		"project:kubernetes:deployment:delete",
		"project:kubernetes:ingress:create",
		"project:kubernetes:ingress:update",
		"project:kubernetes:ingress:delete",
		"project:kubernetes:ingressroute:create",
		"project:kubernetes:ingressroute:update",
		"project:kubernetes:ingressroute:delete",
		"project:kubernetes:ingresskong:create",
		"project:kubernetes:ingresskong:update",
		"project:kubernetes:ingresskong:delete",
		"project:kubernetes:loadbalancer:create",
		"project:kubernetes:loadbalancer:update",
		"project:kubernetes:loadbalancer:delete",
		"project:kubernetes:redis:create",
		"project:kubernetes:redis:update",
		"project:kubernetes:redis:delete",
		"release:kubernetes:deployment:create",
	}
}

func (x *Kubernetes) Process(e transistor.Event) error {
	log.Debug("Processing kubernetes event")

	if e.Matches(".*:kubernetes:deployment") == true {
		x.ProcessDeployment(e)
		return nil
	}

	if e.Matches(".*:kubernetes:loadbalancer") == true {
		x.ProcessLoadBalancer(e)
		return nil
	}

	if e.Matches(".*kubernetes:ingress:") == true {
		x.ProcessIngressRoute(e)
		return nil
	}

	if e.Matches(".*kubernetes:ingressroute:") == true {
		x.ProcessIngressRoute(e)
		return nil
	}

	if e.Matches(".*kubernetes:ingresskong:") == true {
		x.ProcessKongIngress(e)
		return nil
	}

	if e.Matches(".*kubernetes:redis:") == true {
		x.ProcessRedis(e)
		return nil
	}

	return nil
}

func (x *Kubernetes) sendSuccessResponse(e transistor.Event, state transistor.State, artifacts []transistor.Artifact) {
	event := e.NewEvent(transistor.GetAction("status"), transistor.GetState("complete"), fmt.Sprintf("%s has completed successfully", e.Event()))
	event.Artifacts = artifacts

	x.events <- event
}

func (x *Kubernetes) sendErrorResponse(e transistor.Event, msg string) {
	event := e.NewEvent(transistor.GetAction("status"), transistor.GetState("failed"), msg)
	x.events <- event
}

func (x *Kubernetes) sendInProgress(e transistor.Event, msg string) {
	event := e.NewEvent(transistor.GetAction("status"), transistor.GetState("running"), msg)
	x.events <- event
}

func (x *Kubernetes) GenNamespaceName(suggestedEnvironment string, projectSlug string) string {
	return fmt.Sprintf("%s-%s", suggestedEnvironment, projectSlug)
}

func (x *Kubernetes) GenDeploymentName(slugName string, serviceName string) string {
	return slugName + "-" + serviceName
}

func (x *Kubernetes) GenOneShotServiceName(slugName string, serviceName string) string {
	return "os-" + slugName + "-" + serviceName
}

func (x *Kubernetes) CreateNamespaceIfNotExists(namespace string, coreInterface corev1.CoreV1Interface) error {
	// Create namespace if it does not exist.
	_, nameGetErr := coreInterface.Namespaces().Get(namespace, meta_v1.GetOptions{})
	if nameGetErr != nil {
		if k8s_errors.IsNotFound(nameGetErr) {
			log.Warn(fmt.Sprintf("Namespace %s does not yet exist, creating.", namespace))
			namespaceParams := &v1.Namespace{
				TypeMeta: meta_v1.TypeMeta{
					Kind:       "Namespace",
					APIVersion: "v1",
				},
				ObjectMeta: meta_v1.ObjectMeta{
					Name: namespace,
				},
			}
			_, createNamespaceErr := coreInterface.Namespaces().Create(namespaceParams)
			if createNamespaceErr != nil {
				log.Error(fmt.Sprintf("Error '%s' creating namespace %s", createNamespaceErr, namespace))
				return errors.Wrap(createNamespaceErr, 1)
			}
			log.Debug(fmt.Sprintf("Namespace created: %s", namespace))
		} else {
			log.Error(fmt.Sprintf("Unhandled error occured looking up namespace %s: '%s'", namespace, nameGetErr))
			return errors.Wrap(nameGetErr, 1)
		}
	}
	return nil
}

func (x *Kubernetes) GetTempDir() (string, error) {
	for {
		filePath := fmt.Sprintf("/tmp/%s", uuid.NewV1().String())
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			log.Warn("directory does not exist, creating.")
			// create the file
			err = os.MkdirAll(filePath, os.ModeDir|0700)
			if err != nil {
				log.Error(err.Error())
				return "", errors.Wrap(err, 1)
			}
			return filePath, nil
		}
	}
}

// Build a KubeConfig file and save to temporary directory
// Use this file later when building the clientset
func (x *Kubernetes) setupKubeConfig(e transistor.Event) (bool, string, error) {
	randomDirectory, err := x.GetTempDir()
	if err != nil {
		log.Error(err.Error())
		return false, "", err
	}

	kubeconfig, err := e.GetArtifact("kubeconfig")
	if err != nil {
		return false, "", err
	}

	// Convert the kubeconfig into a byte for byte representation
	// so if ANYTHING changes in the file, rebuild the clientset
	// a string check here is not sufficient to ensure the file has not changed
	kubeconfig_md5_raw := md5.Sum([]byte(kubeconfig.String()))
	kubeconfig_md5 := hex.EncodeToString(kubeconfig_md5_raw[:])

	clientCert, err := e.GetArtifact("client_certificate")
	if err != nil {
		return false, "", err
	}

	clientKey, err := e.GetArtifact("client_key")
	if err != nil {
		return false, "", err
	}

	certificateAuthority, err := e.GetArtifact("certificate_authority")
	if err != nil {
		return false, "", err
	}

	err = ioutil.WriteFile(fmt.Sprintf("%s/kubeconfig", randomDirectory), []byte(kubeconfig.String()), 0644)
	if err != nil {
		log.Error(err.Error())
		return false, "", err
	}

	log.Info("Using kubeconfig file: ", fmt.Sprintf("%s/kubeconfig", randomDirectory))

	// generate client cert, client key
	// certificate authority
	err = ioutil.WriteFile(fmt.Sprintf("%s/admin.pem", randomDirectory),
		[]byte(clientCert.String()), 0644)
	if err != nil {
		log.Error(fmt.Sprintf("ERROR: %s", err.Error()))
		return false, "", errors.Wrap(err, 1)
	}

	err = ioutil.WriteFile(fmt.Sprintf("%s/admin-key.pem", randomDirectory),
		[]byte(clientKey.String()), 0644)
	if err != nil {
		log.Error(fmt.Sprintf("ERROR: %s", err.Error()))
		return false, "", errors.Wrap(err, 1)
	}

	err = ioutil.WriteFile(fmt.Sprintf("%s/ca.pem", randomDirectory),
		[]byte(certificateAuthority.String()), 0644)
	if err != nil {
		log.Error(fmt.Sprintf("ERROR: %s", err.Error()))
		return false, "", errors.Wrap(err, 1)
	}

	reuse := true
	if kubeconfig_md5 != x.KubernetesConfig.Hash {
		x.KubernetesConfig.Hash = kubeconfig_md5
		log.Warn("Updated the hash for kubeconfig file ", kubeconfig_md5)

		reuse = false
	}

	return reuse, fmt.Sprintf("%s/kubeconfig", randomDirectory), nil
}

// Builds a Kube ClientSet. Depends on setupKubeConfig to build the
// config file for interacting with K8s
func (x *Kubernetes) SetupClientset(e transistor.Event) (kubernetes.Interface, error) {
	log.Debug("*** BUILDING CLIENTSET ***")

	reuse, kubeconfig, err := x.setupKubeConfig(e)
	if err != nil {
		return nil, err
	}

	if reuse == false || x.KubernetesClient == nil {
		log.Debug("Creating new ClientConfig")
		config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
			&clientcmd.ConfigOverrides{Timeout: "60"}).ClientConfig()

		if err != nil {
			log.Error(fmt.Errorf("ERROR: %s; you must set the environment variable CF_PLUGINS_KUBEDEPLOY_KUBECONFIG=/path/to/kubeconfig", err.Error()))
			return nil, errors.New(ErrKubernetesClientSetup)
		}

		clientset, err := x.K8sNamespacer.NewForConfig(config)
		if err != nil {
			log.Error(fmt.Errorf("ERROR: %s; setting NewForConfig", err.Error()))
			return nil, errors.New(ErrKubernetesNewForConfig)
		}

		x.KubernetesClient = clientset
	} else {
		log.Debug("Reusing the existing kubernetes client / interface!")
	}

	return x.KubernetesClient, nil
}

func (x *Kubernetes) getClientConfig(e transistor.Event) (*rest.Config, error) {
	_, kubeconfig, err := x.setupKubeConfig(e)
	if err != nil {
		log.Error(err.Error())
		return nil, err
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{Timeout: "60"}).ClientConfig()

	if err != nil {
		failMessage := fmt.Sprintf("ERROR getClientConfig: %s; you must set the environment variable CF_PLUGINS_KUBEDEPLOY_KUBECONFIG=/path/to/kubeconfig", err.Error())
		log.Error(failMessage)
		return nil, errors.Wrap(err, 1)
	}

	return config, nil
}

func (x *Kubernetes) getContourClient(e transistor.Event) (contour_client.Interface, error) {
	if x.ContourClient != nil {
		return x.ContourClient, nil
	}

	config, err := x.getClientConfig(e)
	if err != nil {
		return nil, err
	}

	clientset, err := x.K8sContourNamespacer.NewForConfig(config)
	if err != nil {
		failMessage := fmt.Sprintf("ERROR: %s; setting NewForConfig", err.Error())
		log.Error(failMessage)
		return nil, errors.Wrap(err, 1)
	}

	x.ContourClient = clientset
	return x.ContourClient, nil
}
