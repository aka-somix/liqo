package advertisement_operator

import (
	"context"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/fields"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	protocolv1 "github.com/netgroup-polito/dronev2/api/advertisement-operator/v1"
	pkg "github.com/netgroup-polito/dronev2/pkg/advertisement-operator"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	resourcehelper "k8s.io/kubectl/pkg/util/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	log logr.Logger
)

// start the broadcaster which sends Advertisement messages
// it reads the ConfigMaps to get the kubeconfigs to the remote clusters and create a client for each of them
// parameters
// - clusterId: the cluster ID of your cluster (must be a UUID)
// - localKubeconfig: the path to the kubeconfig of the local cluster. Set it only when you are debugging and need to launch the program as a process and not inside Kubernetes
// - foreignKubeconfig: the path to the kubeconfig of the foreign cluster. Set it only when you are debugging and need to launch the program as a process and not inside Kubernetes
// - gatewayIP: the IP address of the gateway node
// - gatewayPrivateIP: the private IP address of the gateway node
func StartBroadcaster(clusterId string, localKubeconfig string, foreignKubeconfig string, gatewayIP string, gatewayPrivateIP string) {
	log = ctrl.Log.WithName("advertisement-broadcaster")
	log.Info("starting broadcaster")

	// get a client to the local cluster
	localClient, err := pkg.NewK8sClient(localKubeconfig, nil)
	if err != nil {
		log.Error(err, "Unable to create client to local cluster")
		return
	}
	// TODO: maybe we can use only the CRD client
	localCRDClient, err := pkg.NewCRDClient(localKubeconfig, nil)
	if err != nil {
		log.Error(err, "Unable to create client to local cluster")
		return
	}

	// get configMaps containing the kubeconfig of the foreign clusters
	configMaps, err := localClient.CoreV1().ConfigMaps("default").List(metav1.ListOptions{})
	if err != nil {
		log.Error(err, "Unable to list configMaps")
		return
	}

	var wg sync.WaitGroup
	// during operation the foreignKubeconfigs are taken from the ConfigMaps
	for _, cm := range configMaps.Items {
		if strings.HasPrefix(cm.Name, "foreign-kubeconfig") {
			wg.Add(1)
			go GenerateAdvertisement(&wg, localClient, localCRDClient, foreignKubeconfig, cm.DeepCopy(), clusterId, gatewayIP, gatewayPrivateIP)
		}
	}

	wg.Wait()
}

// generate an advertisement message every 10 minutes and post it to remote clusters
// parameters
// - localClient: a client to the local kubernetes
// - localCRDClient: a CRD client to the local kubernetes
// - foreignKubeconfigPath: the path to a kubeconfig file. If set, this file is used to create a client to the foreign cluster. Set it only for debugging purposes
// - cm: the configMap containing the kubeconfig to the foreign cluster. IMPORTANT: the data in the configMap must be named "remote"
func GenerateAdvertisement(wg *sync.WaitGroup, localClient *kubernetes.Clientset, localCRDClient client.Client, foreignKubeconfigPath string, cm *v1.ConfigMap, clusterId string, gatewayIP string, gatewayPrivateIP string) {
	//TODO: recovering logic if errors occurs

	var remoteClient client.Client
	var err error
	var retry int
	var foreignClusterId string
	var once sync.Once

	defer wg.Done()
	// extract the foreign cluster id from the configMap
	if cm != nil {
		foreignClusterId = cm.Name[len("foreign-kubeconfig-"):]
	}
	// create a CRDclient to the foreign cluster
	for retry = 0; retry < 3; retry++ {
		remoteClient, err = pkg.NewCRDClient(foreignKubeconfigPath, cm)
		if err != nil {
			log.Error(err, "Unable to create client to remote cluster "+foreignClusterId+". Retry in 1 minute")
			time.Sleep(1 * time.Minute)
		} else {
			break
		}
	}
	if retry == 3 {
		log.Error(err, "Failed to create client to remote cluster "+foreignClusterId)
		return
	} else {
		log.Info("created client to remote cluster " + foreignClusterId)
	}

	for {
		nodes, err := localClient.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: "type != virtual-node"})
		if err != nil {
			log.Error(err, "Unable to list nodes")
			return
		}

		adv := CreateAdvertisement(nodes.Items, clusterId, gatewayIP, gatewayPrivateIP)
		err = pkg.CreateOrUpdate(remoteClient, context.Background(), log, adv)
		if err != nil {
			log.Error(err, "Unable to create advertisement on remote cluster "+foreignClusterId)
		} else {
			log.Info("correctly created advertisement on remote cluster " + foreignClusterId)
			// the watcher must be launched only once
			go once.Do(func() {
				scheme := runtime.NewScheme()
				_ = clientgoscheme.AddToScheme(scheme)
				_ = protocolv1.AddToScheme(scheme)
				WatchAdvertisement(localCRDClient, scheme, foreignKubeconfigPath, cm, clusterId, foreignClusterId)
			})
		}
		time.Sleep(10 * time.Minute)
	}
}

// create advertisement message
func CreateAdvertisement(nodes []corev1.Node, clusterId string, gatewayIP string, gatewayPrivateIp string) protocolv1.Advertisement {

	availability, images := GetClusterResources(nodes)
	prices := ComputePrices(images)

	adv := protocolv1.Advertisement{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "advertisement-" + clusterId,
			Namespace: "default",
		},
		Spec: protocolv1.AdvertisementSpec{
			ClusterId:    clusterId,
			Images:       images,
			Availability: availability,
			LimitRange: v1.LimitRangeSpec{
				Limits: []v1.LimitRangeItem{},
			},
			Prices:       prices,
			Network: protocolv1.NetworkInfo{
				PodCIDR:            GetPodCIDR(nodes),
				GatewayIP:          gatewayIP,
				GatewayPrivateIP:   gatewayPrivateIp,
				SupportedProtocols: nil,
			},
			Timestamp:  metav1.NewTime(time.Now()),
			TimeToLive: metav1.NewTime(time.Now().Add(30 * time.Minute)),
		},
	}
	return adv
}

func GetPodCIDR(nodes []corev1.Node) string {
	var podCIDR string
	token := strings.Split(nodes[0].Spec.PodCIDR, ".")
	if len(token) >= 2 {
		podCIDR = token[0] + "." + token[1] + "." + "0" + "." + "0/16"
	} else {
		podCIDR = "172.17.0.0/16"
	}
	return podCIDR
}

func GetGateway(nodes []corev1.Node) string {
	return nodes[0].Status.Addresses[0].Address
}

func GetGatewayPrivateIP() string {
	//TODO: implement

	return ""
}

func getPodsTotalRequestsAndLimits(podList *corev1.PodList) (reqs map[corev1.ResourceName]resource.Quantity, limits map[corev1.ResourceName]resource.Quantity) {
	reqs, limits = map[corev1.ResourceName]resource.Quantity{}, map[corev1.ResourceName]resource.Quantity{}
	for _, pod := range podList.Items {
		podReqs, podLimits := resourcehelper.PodRequestsAndLimits(&pod)
		for podReqName, podReqValue := range podReqs {
			if value, ok := reqs[podReqName]; !ok {
				reqs[podReqName] = podReqValue.DeepCopy()
			} else {
				value.Add(podReqValue)
				reqs[podReqName] = value
			}
		}
		for podLimitName, podLimitValue := range podLimits {
			if value, ok := limits[podLimitName]; !ok {
				limits[podLimitName] = podLimitValue.DeepCopy()
			} else {
				value.Add(podLimitValue)
				limits[podLimitName] = value
			}
		}
	}
	return
}

func A(c *kubernetes.Clientset, namespace string, name string) (string, error){
	fieldSelector, err := fields.ParseSelector("spec.nodeName=" + name + ",status.phase!=" + string(corev1.PodSucceeded) + ",status.phase!=" + string(corev1.PodFailed))
	if err != nil {
		return "", err
	}
	nodeNonTerminatedPodsList, err := c.CoreV1().Pods(namespace).List(metav1.ListOptions{FieldSelector: fieldSelector.String()})
	if err != nil {
		if !errors.IsForbidden(err) {
			return "", err
		}

	}
	reqs, limits := getPodsTotalRequestsAndLimits(nodeNonTerminatedPodsList)
	cpuReqs, cpuLimits, memoryReqs, memoryLimits, ephemeralstorageReqs, ephemeralstorageLimits :=
		reqs[corev1.ResourceCPU], limits[corev1.ResourceCPU], reqs[corev1.ResourceMemory], limits[corev1.ResourceMemory], reqs[corev1.ResourceEphemeralStorage], limits[corev1.ResourceEphemeralStorage]

}

// get cluster resources (cpu, ram and pods) and images
func GetClusterResources(nodes []corev1.Node) (corev1.ResourceList, []corev1.ContainerImage) {
	cpu := resource.Quantity{}
	ram := resource.Quantity{}
	pods := resource.Quantity{}
	images := make([]corev1.ContainerImage, 0)


	for _, node := range nodes {
		cpu.Add(*node.Status.Allocatable.Cpu())
		ram.Add(*node.Status.Allocatable.Memory())
		pods.Add(*node.Status.Allocatable.Pods())

		//TODO: filter images
		for _, image := range node.Status.Images {
			images = append(images, image)
		}
	}
	availability := corev1.ResourceList{}
	availability[corev1.ResourceCPU] = cpu
	availability[corev1.ResourceMemory] = ram
	availability[corev1.ResourcePods] = pods
	return availability, images
}

// create prices resource for advertisement
func ComputePrices(images []corev1.ContainerImage) corev1.ResourceList {
	//TODO: logic to set prices
	prices := corev1.ResourceList{}
	prices[corev1.ResourceCPU] = *resource.NewQuantity(1, resource.DecimalSI)
	prices[corev1.ResourceMemory] = resource.MustParse("2Gi")
	for _, image := range images {
		for _, name := range image.Names {
			prices[corev1.ResourceName(name)] = *resource.NewQuantity(5, resource.DecimalSI)
		}
	}
	return prices
}
