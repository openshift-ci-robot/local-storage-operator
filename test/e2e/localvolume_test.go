package e2e

import (
	"context"
	goctx "context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/onsi/gomega"
	operatorv1 "github.com/openshift/api/operator/v1"
	localv1 "github.com/openshift/local-storage-operator/pkg/apis/local/v1"
	"github.com/openshift/local-storage-operator/pkg/common"
	commontypes "github.com/openshift/local-storage-operator/pkg/common"
	"github.com/openshift/local-storage-operator/pkg/controller/nodedaemon"
	framework "github.com/operator-framework/operator-sdk/pkg/test"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	dynclient "sigs.k8s.io/controller-runtime/pkg/client"
	provCommon "sigs.k8s.io/sig-storage-local-static-provisioner/pkg/common"
)

var (
	awsEBSNitroRegex  = "^[cmr]5.*|t3|z1d"
	labelInstanceType = "beta.kubernetes.io/instance-type"
	pvConsumerLabel   = "pv-consumer"
)

func LocalVolumeTest(ctx *framework.Context, cleanupFuncs *[]cleanupFn) func(*testing.T) {
	return func(t *testing.T) {
		f := framework.Global
		namespace, err := ctx.GetNamespace()
		if err != nil {
			t.Fatalf("error fetching namespace : %v", err)
		}

		// get nodes
		nodeList := &corev1.NodeList{}
		err = f.Client.List(context.TODO(), nodeList, client.HasLabels{labelNodeRoleWorker})
		if err != nil {
			t.Fatalf("failed to list nodes: %+v", err)
		}

		minNodes := 3
		if len(nodeList.Items) < minNodes {
			t.Fatalf("expected to have at least %d nodes", minNodes)
		}

		// represents the disk layout to setup on the nodes.
		nodeEnv := []nodeDisks{
			{
				disks: []disk{
					{size: 10},
					{size: 20},
				},
				node: nodeList.Items[0],
			},
			{
				disks: []disk{
					{size: 10},
					{size: 20},
				},
				node: nodeList.Items[1],
			},
		}
		selectedNode := nodeEnv[0].node

		matcher := gomega.NewGomegaWithT(t)
		gomega.SetDefaultEventuallyTimeout(time.Minute * 10)
		gomega.SetDefaultEventuallyPollingInterval(time.Second * 2)

		t.Log("getting AWS region info from node spec")
		_, region, _, err := getAWSNodeInfo(nodeList.Items[0])
		matcher.Expect(err).NotTo(gomega.HaveOccurred(), "getAWSNodeInfo")

		// initialize client
		t.Log("initialize ec2 creds")
		ec2Client, err := getEC2Client(region)
		matcher.Expect(err).NotTo(gomega.HaveOccurred(), "getEC2Client")

		// cleanup host dirs
		addToCleanupFuncs(cleanupFuncs, "cleanupSymlinkDir", func(t *testing.T) error {
			return cleanupSymlinkDir(t, ctx, nodeEnv)
		})
		// register disk cleanup
		addToCleanupFuncs(cleanupFuncs, "cleanupAWSDisks", func(t *testing.T) error {
			return cleanupAWSDisks(t, ec2Client)
		})

		// create and attach volumes
		t.Log("creating and attaching disks")
		err = createAndAttachAWSVolumes(t, ec2Client, ctx, namespace, nodeEnv)
		matcher.Expect(err).NotTo(gomega.HaveOccurred(), "createAndAttachAWSVolumes: %+v", nodeEnv)

		// get the device paths and IDs
		nodeEnv = populateDeviceInfo(t, ctx, nodeEnv)

		selectedDisk := nodeEnv[0].disks[0]
		matcher.Expect(selectedDisk.path).ShouldNot(gomega.BeZero(), "device path should not be empty")

		localVolume := getFakeLocalVolume(selectedNode, selectedDisk.path, namespace)

		matcher.Eventually(func() error {
			t.Log("creating localvolume")
			return f.Client.Create(goctx.TODO(), localVolume, &framework.CleanupOptions{TestContext: ctx})
		}, time.Minute, time.Second*2).ShouldNot(gomega.HaveOccurred(), "creating localvolume")

		// add pv and storageclass cleanup
		addToCleanupFuncs(
			cleanupFuncs,
			"cleanupLVResources",
			func(t *testing.T) error {
				return cleanupLVResources(t, f, localVolume)
			},
		)
		err = waitForDaemonSet(t, f.KubeClient, namespace, nodedaemon.DiskMakerName, retryInterval, timeout)
		if err != nil {
			t.Fatalf("error waiting for diskmaker daemonset : %v", err)
		}

		err = verifyLocalVolume(t, localVolume, f.Client)
		if err != nil {
			t.Fatalf("error verifying localvolume cr: %v", err)
		}

		err = checkLocalVolumeStatus(t, localVolume)
		if err != nil {
			t.Fatalf("error checking localvolume condition: %v", err)
		}

		pvs := eventuallyFindPVs(t, f, localVolume.Spec.StorageClassDevices[0].StorageClassName, 1)
		var expectedPath string
		if len(pvs) > 0 {
			if selectedDisk.id != "" {
				expectedPath = selectedDisk.id
			} else {
				expectedPath = selectedDisk.name
			}
		} else {
			t.Fatalf("no pvs returned by eventuallyFindPVs: %+v", pvs)
		}
		matcher.Expect(filepath.Base(pvs[0].Spec.Local.Path)).To(gomega.Equal(expectedPath))

		// verify pv annotation
		t.Logf("looking for %q annotation on pvs", provCommon.AnnProvisionedBy)
		verifyProvisionerAnnotation(t, pvs, nodeList.Items)

		// verify deletion
		for _, pv := range pvs {
			eventuallyDelete(t, &pv)
		}
		// verify that PVs come back after deletion
		pvs = eventuallyFindPVs(t, f, localVolume.Spec.StorageClassDevices[0].StorageClassName, 1)

		// consume pvs
		consumingObjectList := make([]runtime.Object, 0)
		for _, pv := range pvs {
			pvc, job, pod := consumePV(t, ctx, pv)
			consumingObjectList = append(consumingObjectList, job, pvc, pod)
		}
		// release pvs
		eventuallyDelete(t, consumingObjectList...)

		// verify that PVs eventually come back
		matcher.Eventually(func() bool {

			newPVs := eventuallyFindPVs(t, f, localVolume.Spec.StorageClassDevices[0].StorageClassName, 1)
			for _, pv := range pvs {
				pvFound := false
				for _, newPV := range newPVs {
					if pv.Name == newPV.Name {
						if newPV.Status.Phase == corev1.VolumeAvailable {
							pvFound = true
						} else {
							t.Logf("PV is in phase %q, waiting for it to be in phase %q", newPV.Status.Phase, corev1.VolumeAvailable)
						}
						break
					}
				}
				// expect to find each pv
				if !pvFound {
					return false
				}
			}
			return true
		}, time.Minute*5, time.Second*5).Should(gomega.BeTrue(), "waiting for PVs to become available again")

		// consume one PV
		consumingObjectList = make([]runtime.Object, 0)

		addToCleanupFuncs(cleanupFuncs, "pv-consumer", func(t *testing.T) error {
			eventuallyDelete(t, consumingObjectList...)
			return nil
		})
		for _, pv := range pvs[:1] {
			pvc, job, pod := consumePV(t, ctx, pv)
			consumingObjectList = append(consumingObjectList, job, pvc, pod)
		}
		// attempt localVolume deletion
		matcher.Eventually(func() error {
			t.Logf("deleting LocalVolume %q", localVolume.Name)
			return f.Client.Delete(context.TODO(), localVolume, dynclient.PropagationPolicy(metav1.DeletePropagationBackground))
		}, time.Minute*5, time.Second*5).ShouldNot(gomega.HaveOccurred(), "deleting LocalVolume %q", localVolume.Name)

		// verify finalizer not removed with while bound pvs exists
		matcher.Consistently(func() bool {
			t.Logf("verifying finalizer still exists")
			err := f.Client.Get(goctx.TODO(), types.NamespacedName{Name: localVolume.Name, Namespace: f.Namespace}, localVolume)
			if err != nil && (errors.IsGone(err) || errors.IsNotFound(err)) {
				t.Fatalf("LocalVolume deleted with bound PVs")
				return false
			} else if err != nil {
				t.Logf("error getting LocalVolume: %+v", err)
				return false
			}
			return len(localVolume.ObjectMeta.Finalizers) > 0
		}, time.Second*30, time.Second*5).Should(gomega.BeTrue(), "checking finalizer exists with bound PVs")
		// release PV
		t.Logf("releasing pvs")
		eventuallyDelete(t, consumingObjectList...)
		// verify localVolume deletion
		matcher.Eventually(func() bool {
			t.Log("verifying LocalVolume deletion")
			err := f.Client.Get(goctx.TODO(), types.NamespacedName{Name: localVolume.Name, Namespace: f.Namespace}, localVolume)
			if err != nil && (errors.IsGone(err) || errors.IsNotFound(err)) {
				t.Logf("LocalVolume deleted: %+v", err)
				return true
			} else if err != nil {
				t.Logf("error getting LocalVolume: %+v", err)
				return false
			}
			t.Logf("LocalVolume found: %q with finalizers: %+v", localVolume.Name, localVolume.ObjectMeta.Finalizers)
			return false
		}).Should(gomega.BeTrue(), "verifying LocalVolume has been deleted", localVolume.Name)
	}

}

func consumePV(t *testing.T, ctx *framework.Context, pv corev1.PersistentVolume) (*corev1.PersistentVolumeClaim, *batchv1.Job, *corev1.Pod) {
	matcher := gomega.NewWithT(t)
	f := framework.Global
	name := fmt.Sprintf("%s-consumer", pv.ObjectMeta.Name)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeMode:       pv.Spec.VolumeMode,
			StorageClassName: &pv.Spec.StorageClassName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: pv.Spec.Capacity[corev1.ResourceStorage],
				},
			},
		},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.Namespace,
			Labels: map[string]string{
				"app":     pvConsumerLabel,
				"pv-name": pv.Name,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":     pvConsumerLabel,
						"pv-name": pv.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:  "busybox",
							Image: "gcr.io/google_containers/busybox",
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/data",
									Name:      "volume-to-debug",
								},
							},
							Command: []string{"/bin/sh", "-c"},
							Args: []string{
								"dd if=/dev/random of=/tmp/random.img bs=512 count=1",     // create a new file named random.img
								"md5VAR1=$(md5sum /tmp/random.img | awk '{ print $1 }')",  // calculate md5sum of random.img
								"cp /tmp/random.img /data/random.img",                     // copy random.img file to pvc mountpoint
								"md5VAR2=$(md5sum /data/random.img | awk '{ print $1 }')", // recalculate md5sum of file random.img stored in pvc
								"if [[ \"$md5VAR1\" != \"$md5VAR2\" ]];then exit 1; fi",   // verifies that the md5sum hasn't changed
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "volume-to-debug",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvc.Name,
								},
							},
						},
					},
				},
			},
		},
	}

	// create pvc
	matcher.Eventually(func() error {
		t.Logf("creating pvc: %q", pvc.Name)
		return f.Client.Create(goctx.TODO(), pvc, &framework.CleanupOptions{TestContext: ctx})
	}, time.Minute, time.Second*2).ShouldNot(gomega.HaveOccurred(), "creating pvc")

	// recording a time before the job was created
	toRound := time.Now()
	// rounding down to the same granularity as the timestamp
	timeStarted := time.Date(toRound.Year(), toRound.Month(), toRound.Day(), toRound.Hour(), toRound.Minute(), 0, 0, toRound.Location())

	// create consuming job
	matcher.Eventually(func() error {
		t.Logf("creating job: %q", job.Name)
		return f.Client.Create(goctx.TODO(), job, &framework.CleanupOptions{TestContext: ctx})
	}, time.Minute, time.Second*2).ShouldNot(gomega.HaveOccurred(), "creating job")

	// wait for job to complete
	matcher.Eventually(func() int32 {
		t.Log("waiting for job to complete")
		err := f.Client.Get(goctx.TODO(), types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, job)
		if err != nil {
			t.Logf("error fetching job: %+v", err)
			return 0
		}
		t.Logf("job completions: %d", job.Status.Succeeded)
		return job.Status.Succeeded
	}, time.Minute*5, time.Second*2).Should(gomega.BeNumerically(">=", 1), "waiting for job to complete")

	// return pods because they have to be deleted before pv is released
	podList := &corev1.PodList{}
	var matchingPod corev1.Pod
	matcher.Eventually(func() error {
		t.Logf("looking for the completed pod")
		appLabelReq, err := labels.NewRequirement("app", selection.Equals, []string{pvConsumerLabel})
		if err != nil {
			t.Logf("failed to compose labelselector 'app' requirement: %+v", err)
			return err
		}
		pvNameReq, err := labels.NewRequirement("pv-name", selection.Equals, []string{pv.Name})
		if err != nil {
			t.Logf("failed to compose labelselector 'pv-name' requirement: %+v", err)
			return err
		}
		selector := labels.NewSelector().Add(*appLabelReq).Add(*pvNameReq)
		err = f.Client.List(goctx.TODO(), podList, dynclient.MatchingLabelsSelector{Selector: selector})
		if err != nil {
			t.Logf("failed to list pods: %+v", err)
			return err
		}
		podNameList := make([]string, 0)
		for _, pod := range podList.Items {
			podNameList = append(podNameList, pod.Name)
			if pod.CreationTimestamp.After(timeStarted) {
				matchingPod = pod
				return nil
			} else {
				t.Logf("pod is old: %q created at %v before e2e started at %v, skipping", pod.Name, timeStarted, pod.CreationTimestamp)
			}
		}
		t.Logf("could not find pod created by this e2e in podList: %+v", podNameList)
		return fmt.Errorf("could not find pod")

	}).ShouldNot(gomega.HaveOccurred(), "fetching consuming pod")

	matchingPod.TypeMeta.Kind = "Pod"
	return pvc, job, &matchingPod
}

func verifyProvisionerAnnotation(t *testing.T, pvs []corev1.PersistentVolume, nodeList []corev1.Node) {
	matcher := gomega.NewWithT(t)
	for _, pv := range pvs {
		hostFound := true
		hostname, found := pv.ObjectMeta.Labels[corev1.LabelHostname]
		if !found {
			t.Fatalf("expected to find %q label on the pv %+v", corev1.LabelHostname, pv)
		}
		for _, node := range nodeList {
			nodeHostName, found := node.ObjectMeta.Labels[corev1.LabelHostname]
			if !found {
				t.Fatalf("expected to find %q label on the node %+v", corev1.LabelHostname, node)
			}
			if hostname == nodeHostName {
				expectedAnnotation := common.GetProvisionedByValue(node)
				actualAnnotation, found := pv.ObjectMeta.Annotations[provCommon.AnnProvisionedBy]
				matcher.Expect(found).To(gomega.BeTrue(), "expected to find annotation %q on pv", provCommon.AnnProvisionedBy)
				matcher.Expect(actualAnnotation).To(gomega.Equal(expectedAnnotation), "expected to find correct annotation value for %q", provCommon.AnnProvisionedBy)
				hostFound = true
				break
			}
		}
		if !hostFound {
			t.Fatalf("did not find a node entry matching this pv: %+v nodeList: %+v", pv, nodeList)
		}
	}

}

func cleanupLVResources(t *testing.T, f *framework.Framework, localVolume *localv1.LocalVolume) error {
	err := deleteResource(localVolume, localVolume.Name, localVolume.Namespace, f.Client)
	if err != nil {
		t.Fatalf("error deleting localvolume: %v", err)
	}
	sc := &storagev1.StorageClass{
		TypeMeta:   metav1.TypeMeta{Kind: localv1.LocalVolumeKind},
		ObjectMeta: metav1.ObjectMeta{Name: localVolume.Spec.StorageClassDevices[0].StorageClassName},
	}
	eventuallyDelete(t, sc)
	pvList := &corev1.PersistentVolumeList{}
	matcher := gomega.NewWithT(t)
	matcher.Eventually(func() error {
		err := f.Client.List(context.TODO(), pvList)
		if err != nil {
			return err
		}
		// kind := pvList.TypeMeta.Kind
		t.Logf("Deleting %d PVs", len(pvList.Items))
		for _, pv := range pvList.Items {
			// pv.TypeMeta.Kind = kind
			eventuallyDelete(t, &pv)
		}
		return nil
	}, time.Minute*3, time.Second*2).ShouldNot(gomega.HaveOccurred(), "cleaning up pvs for lv: %q", localVolume.GetName())

	return nil

}
func verifyLocalVolume(t *testing.T, lv *localv1.LocalVolume, client framework.FrameworkClient) error {
	waitErr := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		objectKey := dynclient.ObjectKey{
			Namespace: lv.Namespace,
			Name:      lv.Name,
		}
		err := client.Get(goctx.TODO(), objectKey, lv)
		if err != nil {
			return false, err
		}
		finaliers := lv.GetFinalizers()
		if len(finaliers) == 0 {
			return false, nil
		}
		t.Log("Local volume verification successful")
		return true, nil
	})
	return waitErr
}

func verifyDaemonSetTolerations(kubeclient kubernetes.Interface, daemonSetName, namespace string, tolerations []v1.Toleration) error {
	dsTolerations := []v1.Toleration{}
	err := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		daemonset, err := kubeclient.AppsV1().DaemonSets(namespace).Get(daemonSetName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		dsTolerations = daemonset.Spec.Template.Spec.Tolerations
		return true, err
	})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(dsTolerations, tolerations) {
		return fmt.Errorf("toleration mismatch between daemonset and localvolume: %v, %v", dsTolerations, tolerations)
	}
	return nil
}

func verifyStorageClassDeletion(scName string, kubeclient kubernetes.Interface) error {
	waitError := wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		_, err = kubeclient.StorageV1().StorageClasses().Get(scName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	return waitError
}

func checkLocalVolumeStatus(t *testing.T, lv *localv1.LocalVolume) error {
	localVolumeConditions := lv.Status.Conditions
	if len(localVolumeConditions) == 0 {
		return fmt.Errorf("expected local volume to have conditions")
	}

	c := localVolumeConditions[0]
	if c.Type != operatorv1.OperatorStatusTypeAvailable || c.Status != operatorv1.ConditionTrue {
		return fmt.Errorf("expected available operator condition got %v", localVolumeConditions)
	}

	if c.LastTransitionTime.IsZero() {
		return fmt.Errorf("expect last transition time to be set")
	}
	t.Log("LocalVolume status verification successful")
	return nil
}

func deleteCreatedPV(t *testing.T, kubeClient kubernetes.Interface, lv *localv1.LocalVolume) error {
	err := kubeClient.CoreV1().PersistentVolumes().DeleteCollection(nil, metav1.ListOptions{LabelSelector: commontypes.GetPVOwnerSelector(lv).String()})
	if err == nil {
		t.Log("PV deletion successful")
	}
	return err
}

func waitForCreatedPV(kubeClient kubernetes.Interface, lv *localv1.LocalVolume) error {
	waitErr := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		pvs, err := kubeClient.CoreV1().PersistentVolumes().List(metav1.ListOptions{LabelSelector: commontypes.GetPVOwnerSelector(lv).String()})
		if err != nil {
			if isRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		if len(pvs.Items) > 0 {
			return true, nil
		}
		return false, nil
	})
	return waitErr
}

func selectNode(t *testing.T, kubeClient kubernetes.Interface) v1.Node {
	nodes, err := kubeClient.CoreV1().Nodes().List(metav1.ListOptions{LabelSelector: "node-role.kubernetes.io/worker"})
	var dummyNode v1.Node
	if err != nil {
		t.Fatalf("error finding worker node with %v", err)
	}

	if len(nodes.Items) != 0 {
		return nodes.Items[0]
	}
	nodeList, err := waitListSchedulableNodes(kubeClient)
	if err != nil {
		t.Fatalf("error listing schedulable nodes : %v", err)
	}
	if len(nodeList.Items) != 0 {
		return nodeList.Items[0]
	}
	t.Fatalf("found no schedulable node")
	return dummyNode
}

func selectDisk(kubeClient kubernetes.Interface, node v1.Node) (string, error) {
	var nodeInstanceType string
	for k, v := range node.ObjectMeta.Labels {
		if k == labelInstanceType {
			nodeInstanceType = v
		}
	}
	if ok, _ := regexp.MatchString(awsEBSNitroRegex, nodeInstanceType); ok {
		return getNitroDisk(kubeClient, node)
	}

	localDisk := os.Getenv("TEST_LOCAL_DISK")
	if localDisk != "" {
		return localDisk, nil
	}
	return "", fmt.Errorf("can not find a suitable disk")
}

func getNitroDisk(kubeClient kubernetes.Interface, node v1.Node) (string, error) {
	return "", fmt.Errorf("unimplemented")
}

func isRetryableAPIError(err error) bool {
	// These errors may indicate a transient error that we can retry in tests.
	if apierrors.IsInternalError(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) || utilnet.IsProbableEOF(err) || utilnet.IsConnectionReset(err) {
		return true
	}
	// If the error sends the Retry-After header, we respect it as an explicit confirmation we should retry.
	if _, shouldRetry := apierrors.SuggestsClientDelay(err); shouldRetry {
		return true
	}
	return false
}

// waitListSchedulableNodes is a wrapper around listing nodes supporting retries.
func waitListSchedulableNodes(c kubernetes.Interface) (*v1.NodeList, error) {
	var nodes *v1.NodeList
	var err error
	if wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		nodes, err = c.CoreV1().Nodes().List(metav1.ListOptions{FieldSelector: fields.Set{
			"spec.unschedulable": "false",
		}.AsSelector().String()})
		if err != nil {
			if isRetryableAPIError(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}) != nil {
		return nodes, err
	}
	return nodes, nil
}

func waitForDaemonSet(t *testing.T, kubeclient kubernetes.Interface, namespace, name string, retryInterval, timeout time.Duration) error {
	nodeCount := 1
	var err error
	err = wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		daemonset, err := kubeclient.AppsV1().DaemonSets(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				t.Logf("Waiting for availability of %s daemonset\n", name)
				return false, nil
			}
			return false, err
		}
		if int(daemonset.Status.NumberReady) == nodeCount {
			return true, nil
		}
		t.Logf("Waiting for full availability of %s daemonset (%d/%d)\n", name, int(daemonset.Status.NumberReady), nodeCount)
		return false, nil
	})
	if err != nil {
		return err
	}
	t.Logf("Daemonset available (%d/%d)\n", nodeCount, nodeCount)
	return nil
}

func waitForNodeTaintUpdate(t *testing.T, kubeclient kubernetes.Interface, node v1.Node, retryInterval, timeout time.Duration) (v1.Node, error) {
	var err error
	var newNode *v1.Node
	name := node.Name
	err = wait.Poll(retryInterval, timeout, func() (done bool, err error) {
		newNode, err = kubeclient.CoreV1().Nodes().Get(name, metav1.GetOptions{})
		newNode.Spec.Taints = node.Spec.Taints
		newNode, err = kubeclient.CoreV1().Nodes().Update(newNode)
		if err != nil {
			t.Logf("Failed to update node %v successfully : %v", name, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return *newNode, err
	}
	t.Logf("Node %v updated successfully\n", name)
	return *newNode, nil
}

func getFakeLocalVolume(selectedNode v1.Node, selectedDisk, namespace string) *localv1.LocalVolume {
	localVolume := &localv1.LocalVolume{
		TypeMeta: metav1.TypeMeta{
			Kind:       "LocalVolume",
			APIVersion: localv1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-local-disk",
			Namespace: namespace,
		},
		Spec: localv1.LocalVolumeSpec{
			NodeSelector: &v1.NodeSelector{
				NodeSelectorTerms: []v1.NodeSelectorTerm{
					{
						MatchFields: []v1.NodeSelectorRequirement{
							{Key: "metadata.name", Operator: v1.NodeSelectorOpIn, Values: []string{selectedNode.Name}},
						},
					},
				},
			},
			Tolerations: []v1.Toleration{
				{
					Key:      "localstorage",
					Value:    "testvalue",
					Operator: "Equal",
				},
			},
			StorageClassDevices: []localv1.StorageClassDevice{
				{
					StorageClassName: "test-local-sc",
					DevicePaths:      []string{selectedDisk},
				},
			},
		},
	}

	return localVolume
}

func deleteResource(obj runtime.Object, namespace, name string, client framework.FrameworkClient) error {
	err := client.Delete(goctx.TODO(), obj)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	waitErr := wait.PollImmediate(retryInterval, timeout, func() (bool, error) {
		objectKey := dynclient.ObjectKey{
			Namespace: namespace,
			Name:      name,
		}
		err := client.Get(goctx.TODO(), objectKey, obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	return waitErr
}
