package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	cloudprovider "k8s.io/cloud-provider"
	nodecontroller "k8s.io/cloud-provider/controllers/node"
	servicecontroller "k8s.io/cloud-provider/controllers/service"
	controllersmetrics "k8s.io/component-base/metrics/prometheus/controllers"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cloud-provider-kind/pkg/constants"
	"sigs.k8s.io/cloud-provider-kind/pkg/container"
	"sigs.k8s.io/cloud-provider-kind/pkg/provider"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/log"
)

type Controller struct {
	kind     *cluster.Provider
	clusters map[string]*ccm
}

type ccm struct {
	factory           informers.SharedInformerFactory
	serviceController *servicecontroller.Controller
	nodeController    *nodecontroller.CloudNodeController
	cancelFn          context.CancelFunc
}

func New(logger log.Logger) *Controller {
	controllersmetrics.Register()
	return &Controller{
		kind: cluster.NewProvider(
			cluster.ProviderWithLogger(logger),
		),
		clusters: make(map[string]*ccm),
	}
}

func (c *Controller) Run(ctx context.Context) {
	defer cleanup()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// get existing kind clusters
		clusters, err := c.kind.List()
		if err != nil {
			klog.Infof("error listing clusters, retrying ...: %v", err)
		}

		// add new ones
		for _, cluster := range clusters {
			select {
			case <-ctx.Done():
				return
			default:
			}

			klog.V(3).Infof("processing cluster %s", cluster)
			_, ok := c.clusters[cluster]
			if ok {
				klog.V(3).Infof("cluster %s already exist", cluster)
				continue
			}

			kubeClient, err := c.getKubeClient(ctx, cluster)
			if err != nil {
				klog.Errorf("Failed to create kubeClient for cluster %s: %v", cluster, err)
				continue
			}

			klog.V(2).Infof("Creating new cloud provider for cluster %s", cluster)
			cloud := provider.New(cluster, c.kind)
			ccm, err := startCloudControllerManager(ctx, cluster, kubeClient, cloud)
			if err != nil {
				klog.Errorf("Failed to start cloud controller for cluster %s: %v", cluster, err)
				continue
			}
			klog.Infof("Starting cloud controller for cluster %s", cluster)
			c.clusters[cluster] = ccm
		}
		// remove expired ones
		clusterSet := sets.New(clusters...)
		for cluster, ccm := range c.clusters {
			_, ok := clusterSet[cluster]
			if !ok {
				klog.Infof("Stopping service controller for cluster %s", cluster)
				ccm.cancelFn()
				delete(c.clusters, cluster)
			}
		}
		time.Sleep(30 * time.Second)
	}
}

// getKubeClient returns a kubeclient depending if the ccm runs inside a container
// inside the same docker network that the kind cluster or run externally in the host
// It tries first to connect to the external endpoint
func (c *Controller) getKubeClient(ctx context.Context, cluster string) (kubernetes.Interface, error) {
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	// try internal first
	for _, internal := range []bool{false, true} {
		kconfig, err := c.kind.KubeConfig(cluster, internal)
		if err != nil {
			klog.Errorf("Failed to get kubeconfig for cluster %s: %v", cluster, err)
			continue
		}

		config, err := clientcmd.RESTConfigFromKubeConfig([]byte(kconfig))
		if err != nil {
			klog.Errorf("Failed to convert kubeconfig for cluster %s: %v", cluster, err)
			continue
		}

		// check that the apiserver is reachable before continue
		// to fail fast and avoid waiting until the client operations timeout
		var ok bool
		for i := 0; i < 5; i++ {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if probeHTTP(httpClient, config.Host) {
				ok = true
				break
			}
			time.Sleep(time.Second * time.Duration(i))
		}
		if !ok {
			klog.Errorf("Failed to connect to apiserver %s: %v", cluster, err)
			continue
		}

		kubeClient, err := kubernetes.NewForConfig(config)
		if err != nil {
			klog.Errorf("Failed to create kubeClient for cluster %s: %v", cluster, err)
			continue
		}
		return kubeClient, err
	}
	return nil, fmt.Errorf("can not find a working kubernetes clientset")
}

func probeHTTP(client *http.Client, address string) bool {
	klog.Infof("probe HTTP address %s", address)
	resp, err := client.Get(address)
	if err != nil {
		klog.Infof("Failed to connect to HTTP address %s: %v", address, err)
		return false
	}
	defer resp.Body.Close()
	// drain the body
	io.ReadAll(resp.Body) // nolint:errcheck
	// we only want to verify connectivity so don't need to check the http status code
	// as the apiserver may not be ready
	return true
}

// TODO: implement leader election to not have problems with  multiple providers
// ref: https://github.com/kubernetes/kubernetes/blob/d97ea0f705847f90740cac3bc3dd8f6a4026d0b5/cmd/kube-scheduler/app/server.go#L211
func startCloudControllerManager(ctx context.Context, clusterName string, kubeClient kubernetes.Interface, cloud cloudprovider.Interface) (*ccm, error) {
	client := kubeClient.Discovery().RESTClient()
	// wait for health
	err := wait.PollImmediateWithContext(ctx, 1*time.Second, 30*time.Second, func(ctx context.Context) (bool, error) {
		healthStatus := 0
		client.Get().AbsPath("/healthz").Do(ctx).StatusCode(&healthStatus)
		if healthStatus != http.StatusOK {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		klog.Errorf("Failed waiting for apiserver to be ready: %v", err)
		return nil, err
	}

	sharedInformers := informers.NewSharedInformerFactory(kubeClient, 60*time.Second)

	ccmMetrics := controllersmetrics.NewControllerManagerMetrics(clusterName)
	// Start the service controller
	serviceController, err := servicecontroller.New(
		cloud,
		kubeClient,
		sharedInformers.Core().V1().Services(),
		sharedInformers.Core().V1().Nodes(),
		clusterName,
		utilfeature.DefaultFeatureGate,
	)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.Errorf("Failed to start service controller: %v", err)
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	go serviceController.Run(ctx, 5, ccmMetrics)

	// Start the node controller
	nodeController, err := nodecontroller.NewCloudNodeController(
		sharedInformers.Core().V1().Nodes(),
		kubeClient,
		cloud,
		30*time.Second,
	)
	if err != nil {
		// This error shouldn't fail. It lives like this as a legacy.
		klog.Errorf("Failed to start node controller: %v", err)
		cancel()
		return nil, err
	}
	go nodeController.Run(ctx.Done(), ccmMetrics)

	sharedInformers.Start(ctx.Done())

	return &ccm{
		factory:           sharedInformers,
		serviceController: serviceController,
		nodeController:    nodeController,
		cancelFn:          cancel}, nil
}

func cleanup() {
	containers, err := container.ListByLabel(constants.NodeCCMLabelKey)
	if err != nil {
		klog.Errorf("can't list containers: %v", err)
		return
	}
	for _, id := range containers {
		if err := container.Delete(id); err != nil {
			klog.Errorf("can't delete container %s: %v", id, err)
		}
	}
}
