package liqo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	configSB "github.com/couchbase/service-broker/pkg/config"
	"github.com/golang/glog"
	discoveryv1alpha1 "github.com/liqotech/liqo/apis/discovery/v1alpha1"
	offloadingv1alpha1 "github.com/liqotech/liqo/apis/offloading/v1alpha1"
	"github.com/liqotech/liqo/pkg/consts"
	discovery "github.com/liqotech/liqo/pkg/discovery"
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

func (liqo *Liqo) PeerAndNamespace(ctx context.Context, ClusterID, ClusterToken, ClusterAuthURL, ClusterName, OffloadingPolicy, UserID, NamespacePrefix string, peeringid, namespace_id int, db *sql.DB) {

	glog.Info("Starting peering")
	// Start peering
	fc, err := liqo.Peer(ctx, ClusterID, ClusterToken, ClusterAuthURL, ClusterName)
	if err != nil {
		// Error while peering
		strErr := fmt.Sprintf("Error while peering: %s", err)
		glog.Info(strErr)
		// Set ready to false and register error into db
		_, err = db.Exec("UPDATE peering SET ready = $1, error = $2 WHERE id = $3", false, strErr, peeringid)
		if err != nil {
			glog.Info("Error while updating peering status: ", err)
		}
		return
	}
	glog.Info("Peering started")
	// Wait for the peering to be established
	err = liqo.Wait(ctx, &fc.Spec.ClusterIdentity)
	if err != nil {
		// Error while waiting for peering
		strErr := fmt.Sprintf("Error while waiting for peering: %s", err)
		glog.Info(strErr)
		_, err = db.Exec("UPDATE peering SET ready = $1, error = $2 WHERE id = $3", false, err.Error(), peeringid)
		if err != nil {
			glog.Info("Error while updating peering status: ", err)
		}
		return
	}
	glog.Info("Peering established")

	// Set peering ready and effective namespace
	_, err = db.Exec("UPDATE peering SET ready = $1 WHERE id = $2", true, peeringid)
	if err != nil {
		strErr := fmt.Sprintf("Error while updating peering status: %s", err)
		glog.Info(strErr)
		_, err = db.Exec("UPDATE peering SET ready = $1, error = $2 WHERE id = $3", false, strErr, peeringid)
		if err != nil {
			glog.Info("Error while updating peering status: ", err)
		}
		return
	}

	// Start with the Namespace
	liqo.NamespaceAndOffload(ctx, db, peeringid, namespace_id, ClusterID, ClusterName, OffloadingPolicy, NamespacePrefix)
}

func (liqo *Liqo) NamespaceAndOffload(ctx context.Context, db *sql.DB, peeringid, namespace_id int, ClusterID, ClusterName, OffloadingPolicy, NamespacePrefix string) {

	// Retrieve ForeignCluster
	var fc, err = liqo.GetForeignCluster(ctx, ClusterID)
	if err != nil {
		strErr := fmt.Sprintf("Error while getting ForeignCluster: %s", err)
		glog.Info(strErr)
		_, err = db.Exec("UPDATE namespaces SET ready = $1, error = $2 WHERE id = $3", false, strErr, namespace_id)
		if err != nil {
			glog.Info("Error while updating namespace status: ", err)
		}
		return
	}

	// Create namespace name as combination of elements
	
	// Namespace name
	var namespace string
	// Create and offload namespace
	if NamespacePrefix == "" {
		namespace = ClusterName + "-" + ClusterID + "-" + strconv.Itoa(namespace_id)
	} else {
		namespace = NamespacePrefix + "-" + ClusterID + "-" + strconv.Itoa(namespace_id)
	}
	
	glog.Info("Creating namespace")
	_, _, err = liqo.OffloadNamespace(ctx, namespace, &fc.Spec.ClusterIdentity, OffloadingPolicy)
	if err != nil {
		strErr := fmt.Sprintf("Error while offloading namespace: %s", err)
		glog.Info(strErr)
		_, err = db.Exec("UPDATE namespaces SET ready = $1, error = $2 WHERE id = $3", false, strErr, namespace_id)
		if err != nil {
			glog.Info("Error while updating namespace status: ", err)
		}
		return
	}
	glog.Info("Namespace offloaded")

	
	// UPDATE namespaces in db
	_, err = db.Exec("UPDATE namespaces SET namespace = $1, ready = $2 WHERE id = $3", namespace, true, namespace_id)
	if err != nil {
		strErr := fmt.Sprintf("Error while updating namespace name: %s", err)
		glog.Info(strErr)
		_, err = db.Exec("UPDATE namespaces SET ready = $1, error = $2 WHERE id = $3", false, strErr, namespace_id)
		if err != nil {
			glog.Info("Error while updating namespace status: ", err)
		}
		return
	}
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

func (liqo *Liqo) GetForeignCluster(ctx context.Context, ClusterID string) (*discoveryv1alpha1.ForeignCluster, error) {
	fc, err := foreigncluster.GetForeignClusterByID(ctx, liqo.CRClient, ClusterID)
	if err != nil {
		if(kerrors.IsNotFound(err)){
			glog.Info("ForeignCluster not found")
			return nil, nil
		}else{
			glog.Info("Error while getting ForeignCluster: ", err )
			return nil, err
		}
	}
	return fc, nil
}

func (liqo *Liqo) CheckIfReady(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) (bool, error) {

	// Check if the remote cluster is authenticated.
	ready, err := liqo.checkAuth(ctx, remoteClusterID)
	if err != nil {
		return false, err
	}
	if !ready {
		glog.Info("Cluster ", remoteClusterID.ClusterName, " is not ready yet cause is not authenticated")
		return false, nil
	}

	// Check if the remote cluster is ready to accept incoming peering.
	ready, err = liqo.checkOutgoingPeering(ctx, remoteClusterID)
	if err != nil {
		return false, err
	}
	if !ready {
		glog.Info("Cluster ", remoteClusterID.ClusterName, " is not ready yet cause outgoing peering is not enabled")
		return false, nil
	}
	
	// Check if the remote cluster network is ready.
	ready, err = liqo.checkNetwork(ctx, remoteClusterID)
	if err != nil {
		return false, err
	}
	if !ready {
		glog.Info("Cluster ", remoteClusterID.ClusterName, " is not ready yet cause network is not ready")
		return false, nil
	}

	// Check if the virtual node is ready.
	ready, err = liqo.checkNode(ctx, remoteClusterID)
	if err != nil {
		return false, err
	}
	if !ready {
		glog.Info("Cluster ", remoteClusterID.ClusterName, " is not ready yet cause virtual node is not ready")
		return false, nil
	}

	return true, nil
}

func (liqo *Liqo) checkAuth(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) (bool, error) {
	remName := remoteClusterID.ClusterName
	glog.Info("Checking authentication to the remote cluster ", remName)
	// Get foreign cluster
	fc, err := foreigncluster.GetForeignClusterByID(ctx, liqo.CRClient, remoteClusterID.ClusterID)
	if err != nil {
		return false, err
	}
	// Check if the authentication is enabled
	return fcutils.IsAuthenticated(fc), nil
}

func (liqo *Liqo) checkOutgoingPeering(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) (bool, error) {
	remName := remoteClusterID.ClusterName
	glog.Info("Checking outgoing peering to the remote cluster ", remName)
	// Get foreign cluster
	fc, err := foreigncluster.GetForeignClusterByID(ctx, liqo.CRClient, remoteClusterID.ClusterID)
	if err != nil {
		return false, err
	}
	// Check if the peering is enabled
	return fcutils.IsOutgoingJoined(fc), nil
}

func (liqo *Liqo) checkNetwork(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) (bool, error) {
	remName := remoteClusterID.ClusterName
	glog.Info("Checking network to the remote cluster ", remName)
	// Get foreign cluster
	fc, err := foreigncluster.GetForeignClusterByID(ctx, liqo.CRClient, remoteClusterID.ClusterID)
	if err != nil {
		return false, err
	}
	// Check if the network is ready
	return fcutils.IsNetworkingEstablished(fc), nil
}

func (liqo *Liqo) checkNode(ctx context.Context, remoteClusterID *discoveryv1alpha1.ClusterIdentity) (bool, error) {
	remName := remoteClusterID.ClusterName
	glog.Info("Checking node to the remote cluster ", remName)
	// Get the virtual node
	node, err := getters.GetNodeByClusterID(ctx, liqo.CRClient, remoteClusterID)
	if err != nil {
		return false, client.IgnoreNotFound(err)
	}
	// Check if the node is ready
	return utils.IsNodeReady(node), nil
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
	// Define a new namespace
	name := namespace

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
	glog.Info("Offloading policy: ", offloadingPolicy)
	if offloadingPolicy == "Local" {
		pos = offloadingv1alpha1.LocalPodOffloadingStrategyType
	} else if offloadingPolicy == "Remote" {
		pos = offloadingv1alpha1.RemotePodOffloadingStrategyType
	} else if offloadingPolicy == "LocalAndRemote" {
		pos = offloadingv1alpha1.LocalAndRemotePodOffloadingStrategyType
	}
	nsoff := &offloadingv1alpha1.NamespaceOffloading{
		ObjectMeta: metav1.ObjectMeta{Name: consts.DefaultNamespaceOffloadingName, Namespace: name},
		Spec: offloadingv1alpha1.NamespaceOffloadingSpec{
			NamespaceMappingStrategy: nms, PodOffloadingStrategy: pos,
			ClusterSelector: corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      consts.RemoteClusterID,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{remoteClusterID.ClusterID},
							},
						},
					},
				},
			},
		},
	}

	err = liqo.CRClient.Create(ctx, nsoff); if err != nil {
		glog.Info("Error while creating namespaceOffloading: ", err)
		return nil, nil, err
	}

	return ns, nsoff, nil	

}

// GetNamespaceOffloading returns the namespaceOffloading of a namespace.
func (liqo *Liqo) GetNamespaceOffloading(namespace string) (*offloadingv1alpha1.NamespaceOffloading, error) {
	nsoff := &offloadingv1alpha1.NamespaceOffloading{}
	err := liqo.CRClient.Get(context.Background(), client.ObjectKey{Name: consts.DefaultNamespaceOffloadingName, Namespace: namespace}, nsoff)
	if err != nil {
		glog.Info("Error while retrieving namespaceOffloading: ", err)
		return nil, err
	}
	return nsoff, nil

}