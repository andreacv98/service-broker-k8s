package liqo

import (
	"context"
	"fmt"
	"time"

	configSB "github.com/couchbase/service-broker/pkg/config"
	"github.com/golang/glog"
	discoveryv1alpha1 "github.com/liqotech/liqo/apis/discovery/v1alpha1"
	offloadingv1alpha1 "github.com/liqotech/liqo/apis/offloading/v1alpha1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/discovery"
	"github.com/liqotech/liqo/pkg/utils"
	authenticationtokenutils "github.com/liqotech/liqo/pkg/utils/authenticationtoken"
	fcutils "github.com/liqotech/liqo/pkg/utils/foreignCluster"
	foreigncluster "github.com/liqotech/liqo/pkg/utils/foreignCluster"
	"github.com/liqotech/liqo/pkg/utils/getters"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"k8s.io/client-go/kubernetes/scheme"

	corev1 "k8s.io/api/core/v1"

	
    "k8s.io/apimachinery/pkg/labels"
)

type Liqo struct {
	CRClient client.Client
	Namespace string
}

func Create(namespaceLiqo string) (*Liqo, error) {
	// Create controller runtime client for liqo
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	glog.Info("Using config: ", cfg)

	err = discoveryv1alpha1.AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, err
	}
	err = offloadingv1alpha1.AddToScheme(scheme.Scheme)
	if err != nil {
		return nil, err
	}

	CRClient, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, err
	}
	glog.Info("Using client: ", CRClient)

	LiqoNamespace := "liqo"
	glog.Info("Using namespace: ", LiqoNamespace)

	return &Liqo{CRClient: CRClient, Namespace: LiqoNamespace}, nil
}


func (liqo *Liqo) Peer(ctx context.Context, ClusterID, ClusterToken, ClusterAuthURL, ClusterName string) (*discoveryv1alpha1.ForeignCluster, error) {	

	clusterIdentity, err := utils.GetClusterIdentityWithControllerClient(ctx, liqo.CRClient, liqo.Namespace)
	if err != nil {
		return nil, err
	}
	glog.Info("Cluster Identity: ", clusterIdentity)

	// Check whether the cluster IDs are the same, as we cannot peer with ourselves.
	if clusterIdentity.ClusterID == ClusterID {
		return nil, fmt.Errorf("cannot peer with the same cluster")
	}
	glog.Info("Cluster is not the same")

	// Create the secret containing the authentication token.
	err = authenticationtokenutils.StoreInSecret(ctx, configSB.Clients().Kubernetes(), ClusterID, ClusterToken, liqo.Namespace)
	if err != nil {
		return nil, err
	}
	glog.Info("Secret created")

	// Enforce the presence of the ForeignCluster resource.
	return liqo.enforceForeignCluster(ctx, ClusterID, ClusterToken, ClusterAuthURL, ClusterName)
}

func (liqo *Liqo) enforceForeignCluster(ctx context.Context, ClusterID, ClusterToken, ClusterAuthURL, ClusterName string) (*discoveryv1alpha1.ForeignCluster, error) {
	fc, err := foreigncluster.GetForeignClusterByID(ctx, liqo.CRClient, ClusterID)
	if kerrors.IsNotFound(err) {
		glog.Info("ForeignCluster not found, creating it...")
		fc = &discoveryv1alpha1.ForeignCluster{ObjectMeta: metav1.ObjectMeta{Name: ClusterName,
			Labels: map[string]string{discovery.ClusterIDLabel: ClusterID}}}
	} else if err != nil {
		glog.Info("Error while getting ForeignCluster: ", err )
		return nil, err
	}
	glog.Info("ForeignCluster: ", fc)

	_, err = controllerutil.CreateOrUpdate(ctx, liqo.CRClient, fc, func() error {
		if fc.Spec.PeeringType != discoveryv1alpha1.PeeringTypeUnknown && fc.Spec.PeeringType != discoveryv1alpha1.PeeringTypeOutOfBand {
			return fmt.Errorf("a peering of type %s already exists towards remote cluster %q, cannot be changed to %s",
				fc.Spec.PeeringType, ClusterName, discoveryv1alpha1.PeeringTypeOutOfBand)
		}

		fc.Spec.PeeringType = discoveryv1alpha1.PeeringTypeOutOfBand
		fc.Spec.ClusterIdentity.ClusterID = ClusterID
		if fc.Spec.ClusterIdentity.ClusterName == "" {
			fc.Spec.ClusterIdentity.ClusterName = ClusterName
		}

		fc.Spec.ForeignAuthURL = ClusterAuthURL
		fc.Spec.ForeignProxyURL = ""
		fc.Spec.OutgoingPeeringEnabled = discoveryv1alpha1.PeeringEnabledYes
		if fc.Spec.IncomingPeeringEnabled == "" {
			fc.Spec.IncomingPeeringEnabled = discoveryv1alpha1.PeeringEnabledAuto
		}
		if fc.Spec.InsecureSkipTLSVerify == nil {
			fc.Spec.InsecureSkipTLSVerify = pointer.BoolPtr(true)
		}
		glog.Info("ForeignCluster updated: ", fc)
		return nil
	})

	glog.Info("ForeignCluster created: ", fc)

	return fc, err
}

// Wait waits for the peering to the remote cluster to be fully enabled.
func (liqo *Liqo) Wait(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) error {

	if err := liqo.waitForAuth(ctx, remoteClusterID); err != nil {
		return err
	}

	if err := liqo.waitForOutgoingPeering(ctx, remoteClusterID); err != nil {
		return err
	}

	if err := liqo.waitForNetwork(ctx, remoteClusterID); err != nil {
		return err
	}

	return liqo.waitForNode(ctx, remoteClusterID)
}

func (liqo *Liqo) waitForAuth(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) error {
	remName := remoteClusterID.ClusterName
	glog.Info("Waiting for authentication to the remote cluster ", remName)
	err := fcutils.PollForEvent(ctx, liqo.CRClient, remoteClusterID, fcutils.IsAuthenticated, 1*time.Second)
	if err != nil {
		glog.Info("Authentication to the remote cluster ", remName, " failed: ", err)
		return err
	}
	glog.Info("Authenticated to remote cluster ", remName)
	return nil
}

// ForNetwork waits until the networking has been established with the remote cluster or the timeout expires.
func (liqo *Liqo) waitForNetwork(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) error {
	remName := remoteClusterID.ClusterName
	glog.Info("Waiting for network to the remote cluster ", remName)
	err := fcutils.PollForEvent(ctx, liqo.CRClient, remoteClusterID, fcutils.IsNetworkingEstablished, 1*time.Second)
	if err != nil {
		glog.Info("Failed establishing networking to the remote cluster ", remName, ": ", err)
		return err
	}
	glog.Info("Network established to the remote cluster ", remName)
	return nil
}

// ForOutgoingPeering waits until the status on the foreiglcusters resource states that the outgoing peering has been successfully
// established or the timeout expires.
func (liqo *Liqo) waitForOutgoingPeering(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) error {
	remName := remoteClusterID.ClusterName
	glog.Info("Waiting for outgoing peering to be activated to the remote cluster ", remName)
	err := fcutils.PollForEvent(ctx, liqo.CRClient, remoteClusterID, fcutils.IsOutgoingJoined, 1*time.Second)
	if err != nil {
		glog.Info("Failed activating outgoing peering to the remote cluster ", remName, ": ", err)
		return err
	}
	glog.Info("Outgoing peering activated to the remote cluster ", remName)
	return nil
}

// ForNode waits until the node has been added to the cluster or the timeout expires.
func (liqo *Liqo) waitForNode(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) error {
	remName := remoteClusterID.ClusterName
	glog.Info("Waiting for node to be created for the remote cluster ", remName)

	err := wait.PollImmediateUntilWithContext(ctx, 1*time.Second, func(ctx context.Context) (done bool, err error) {
		node, err := getters.GetNodeByClusterID(ctx, liqo.CRClient, remoteClusterID)
		if err != nil {
			return false, client.IgnoreNotFound(err)
		}

		return utils.IsNodeReady(node), nil
	})
	if err != nil {
		glog.Info("Failed waiting for node to be created for the remote cluster ", remName, ": ", err)
		return err
	}
	glog.Info("Node created for the remote cluster ", remName)
	return nil
}

// OffloadNamespace offloads a new namespace to the remote cluster.
func (liqo *Liqo) OffloadNamespace(ctx context.Context, namespace string, remoteClusterID *discoveryv1alpha1.ClusterIdentity, offloadingPolicy string) (*corev1.Namespace, *offloadingv1alpha1.NamespaceOffloading, error) {
	remId := remoteClusterID.ClusterID
	// Define a new namespace
	name := namespace+"-"+remId

	// Retrieve list of namespaces by name of the namespace
	nsList := &corev1.NamespaceList{}
	labelSelector := labels.Set{"name": name}.AsSelector()
	err := liqo.CRClient.List(ctx, nsList, &client.ListOptions{LabelSelector: labelSelector}); if err != nil {
		glog.Info("Error while retrieving namespaces: ", err)
		return nil, nil, err
	}
	glog.Info("Namespace list: ", nsList.Items)
	// Check lenght of the list to see if the namespace already exists
	if len(nsList.Items) > 0 {
		glog.Info("Namespace already exists")
		return nil, nil, fmt.Errorf("namespace already exists")
	}

	// Create the namespace
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	err = liqo.CRClient.Create(ctx, ns); if err != nil {
		glog.Info("Error while creating namespace: ", err)
		return nil, nil, err
	}
	glog.Info("Namespace: ", name, " created")

	// Create the namespaceOffloading
	// EnforceSameNameMappingStrategyType is the default strategy
	nms := offloadingv1alpha1.EnforceSameNameMappingStrategyType
	pos := offloadingv1alpha1.LocalAndRemotePodOffloadingStrategyType
	if offloadingPolicy == "local" {
		pos = offloadingv1alpha1.LocalPodOffloadingStrategyType
	} else if offloadingPolicy == "remote" {
		pos = offloadingv1alpha1.RemotePodOffloadingStrategyType
	} else if offloadingPolicy == "local-remote" {
		pos = offloadingv1alpha1.LocalAndRemotePodOffloadingStrategyType
	}
	nsoff := &offloadingv1alpha1.NamespaceOffloading{
		ObjectMeta: metav1.ObjectMeta{Name: consts.DefaultNamespaceOffloadingName, Namespace: name},
		Spec: offloadingv1alpha1.NamespaceOffloadingSpec{
			NamespaceMappingStrategy: nms, PodOffloadingStrategy: pos,
			ClusterSelector: corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{}}},
	}

	err = liqo.CRClient.Create(ctx, nsoff); if err != nil {
		glog.Info("Error while creating namespaceOffloading: ", err)
		return nil, nil, err
	}

	return ns, nsoff, nil	

}