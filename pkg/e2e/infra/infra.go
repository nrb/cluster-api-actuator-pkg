package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/glog"
	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	e2e "github.com/openshift/cluster-api-actuator-pkg/pkg/e2e/framework"
	mapiv1beta1 "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = g.Describe("[Feature:Machines] Managed cluster should", func() {
	defer g.GinkgoRecover()

	g.It("have machines linked with nodes", func() {
		var err error
		client, err := e2e.LoadClient()
		o.Expect(err).NotTo(o.HaveOccurred())

		o.Expect(isOneMachinePerNode(client)).To(o.BeTrue())
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("have ability to additively reconcile taints", func() {
		var err error
		client, err := e2e.LoadClient()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Verify machine taints are getting applied to node")
		err = func() error {
			listOptions := runtimeclient.ListOptions{
				Namespace: e2e.TestContext.MachineApiNamespace,
			}
			machineList := mapiv1beta1.MachineList{}

			if err := client.List(context.TODO(), &listOptions, &machineList); err != nil {
				return fmt.Errorf("error querying api for machineList object: %v", err)
			}
			g.By("Got the machine list")
			machine := machineList.Items[0]
			if machine.Status.NodeRef == nil {
				return fmt.Errorf("machine %s has no NodeRef", machine.Name)
			}
			g.By(fmt.Sprintf("Got the machine %s", machine.Name))
			nodeName := machine.Status.NodeRef.Name
			nodeKey := types.NamespacedName{
				Namespace: e2e.TestContext.MachineApiNamespace,
				Name:      nodeName,
			}
			node := &corev1.Node{}

			if err := client.Get(context.TODO(), nodeKey, node); err != nil {
				return fmt.Errorf("error querying api for node object: %v", err)
			}
			g.By(fmt.Sprintf("Got the node %s from machine, %s", node.Name, machine.Name))
			nodeTaint := corev1.Taint{
				Key:    "not-from-machine",
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			}
			// Do not remove any taint, just extend the list
			// The test removes the nodes anyway, so the list will not grow over time much
			node.Spec.Taints = append(node.Spec.Taints, nodeTaint)
			if err := client.Update(context.TODO(), node); err != nil {
				return fmt.Errorf("error updating node object with non-machine taint: %v", err)
			}
			g.By("Updated node object with taint")
			machineTaint := corev1.Taint{
				Key:    fmt.Sprintf("from-machine-%v", string(uuid.NewUUID())),
				Value:  "true",
				Effect: corev1.TaintEffectNoSchedule,
			}

			// Do not remove any taint, just extend the list
			// The test removes the machine anyway, so the list will not grow over time much
			machine.Spec.Taints = append(machine.Spec.Taints, machineTaint)
			if err := client.Update(context.TODO(), &machine); err != nil {
				return fmt.Errorf("error updating machine object with taint: %v", err)
			}

			g.By("Updated machine object with taint")
			var expectedTaints = sets.NewString("not-from-machine", machineTaint.Key)
			err := wait.PollImmediate(1*time.Second, e2e.WaitLong, func() (bool, error) {
				if err := client.Get(context.TODO(), nodeKey, node); err != nil {
					glog.Errorf("error querying api for node object: %v", err)
					return false, nil
				}
				glog.Info("Got the node again for verification of taints")
				var observedTaints = sets.NewString()
				for _, taint := range node.Spec.Taints {
					observedTaints.Insert(taint.Key)
				}
				if expectedTaints.Difference(observedTaints).HasAny("not-from-machine", machineTaint.Key) == false {
					glog.Infof("expected : %v, observed %v , difference %v, ", expectedTaints, observedTaints, expectedTaints.Difference(observedTaints))
					return true, nil
				}
				glog.Infof("Did not find all expected taints on the node. Missing: %v", expectedTaints.Difference(observedTaints))
				return false, nil
			})
			return err
		}()
		o.Expect(err).NotTo(o.HaveOccurred())
	})

	g.It("recover from deleted worker machines", func() {
		var err error
		client, err := e2e.LoadClient()
		o.Expect(err).NotTo(o.HaveOccurred())

		// Expect for cluster to cool down from previous tests
		err = e2e.WaitUntilAllNodesAreReady(client)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("checking initial cluster state")
		initialClusterSize, err := getClusterSize(client)
		waitForClusterSizeToBeHealthy(initialClusterSize)

		workerNode, err := getWorkerNode(client)
		o.Expect(err).NotTo(o.HaveOccurred())
		workerMachine, err := getMachineFromNode(client, workerNode)
		o.Expect(err).NotTo(o.HaveOccurred())
		g.By(fmt.Sprintf("deleting machine object %q", workerMachine.Name))
		err = deleteMachine(client, workerMachine)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("waiting for node object %q to go away", workerNode.Name))
		nodeList := corev1.NodeList{}
		o.Eventually(func() bool {
			if err := client.List(context.TODO(), nil, &nodeList); err != nil {
				glog.Errorf("Error querying api for nodeList object: %v, retrying...", err)
				return false
			}
			for _, n := range nodeList.Items {
				if n.Name == workerNode.Name {
					glog.Infof("Node %q still exists. Node conditions are: %v", workerNode.Name, workerNode.Status.Conditions)
					return false
				}
			}
			return true
		}, e2e.WaitLong, 5*time.Second).Should(o.BeTrue())

		g.By(fmt.Sprintf("waiting for new node object to come up"))
		waitForClusterSizeToBeHealthy(initialClusterSize)
	})

	g.It("grow or decrease when scaling out or in", func() {
		g.By("checking initial cluster state")
		client, err := e2e.LoadClient()
		o.Expect(err).NotTo(o.HaveOccurred())

		initialClusterSize, err := getClusterSize(client)
		waitForClusterSizeToBeHealthy(initialClusterSize)

		machineSets, err := getMachineSets(client)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(machineSets)).To(o.BeNumerically(">", 2))
		machineSet := machineSets[0]
		initialReplicasMachineSet := int(pointer.Int32PtrDerefOr(machineSet.Spec.Replicas, 0))
		scaleOut := 3
		scaleIn := initialReplicasMachineSet
		originalReplicas := initialReplicasMachineSet
		clusterGrowth := scaleOut - originalReplicas
		clusterDecrease := scaleOut - scaleIn
		intermediateClusterSize := initialClusterSize + clusterGrowth
		finalClusterSize := initialClusterSize + clusterGrowth - clusterDecrease

		g.By(fmt.Sprintf("scaling out %q machineSet to %d replicas", machineSet.Name, scaleOut))
		err = scaleMachineSet(machineSet.Name, scaleOut)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("waiting for cluster to grow %d nodes. Size should be %d", clusterGrowth, intermediateClusterSize))
		waitForClusterSizeToBeHealthy(intermediateClusterSize)

		g.By(fmt.Sprintf("scaling in %q machineSet to %d replicas", machineSet.Name, scaleIn))
		err = scaleMachineSet(machineSet.Name, scaleIn)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("waiting for cluster to decrease %d nodes. Final size should be %d nodes", clusterDecrease, finalClusterSize))
		waitForClusterSizeToBeHealthy(finalClusterSize)
	})

	g.It("grow and decrease when scaling different machineSets simultaneously", func() {
		client, err := e2e.LoadClient()
		o.Expect(err).NotTo(o.HaveOccurred())
		scaleOut := 3

		g.By("checking initial cluster size")
		initialClusterSize, err := getClusterSize(client)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("getting worker machineSets")
		machineSets, err := getMachineSets(client)
		o.Expect(err).NotTo(o.HaveOccurred())
		o.Expect(len(machineSets)).To(o.BeNumerically(">", 2))
		machineSet0 := machineSets[0]
		initialReplicasMachineSet0 := int(pointer.Int32PtrDerefOr(machineSet0.Spec.Replicas, 0))
		machineSet1 := machineSets[1]
		initialReplicasMachineSet1 := int(pointer.Int32PtrDerefOr(machineSet1.Spec.Replicas, 0))

		g.By(fmt.Sprintf("scaling %q from %d to %d replicas", machineSet0.Name, initialReplicasMachineSet0, scaleOut))
		err = scaleMachineSet(machineSet0.Name, scaleOut)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("scaling %q from %d to %d replicas", machineSet1.Name, initialReplicasMachineSet1, scaleOut))
		err = scaleMachineSet(machineSet1.Name, scaleOut)
		o.Expect(err).NotTo(o.HaveOccurred())

		o.Eventually(func() bool {
			nodes, err := getNodesFromMachineSet(client, machineSet0)
			if err != nil {
				return false
			}
			return len(nodes) == scaleOut && nodesAreReady(nodes)
		}, e2e.WaitLong, 5*time.Second).Should(o.BeTrue())

		o.Eventually(func() bool {
			nodes, err := getNodesFromMachineSet(client, machineSet1)
			if err != nil {
				return false
			}
			return len(nodes) == scaleOut && nodesAreReady(nodes)
		}, e2e.WaitLong, 5*time.Second).Should(o.BeTrue())

		g.By(fmt.Sprintf("scaling %q from %d to %d replicas", machineSet0.Name, scaleOut, initialReplicasMachineSet0))
		err = scaleMachineSet(machineSet0.Name, initialReplicasMachineSet0)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("scaling %q from %d to %d replicas", machineSet1.Name, scaleOut, initialReplicasMachineSet1))
		err = scaleMachineSet(machineSet1.Name, initialReplicasMachineSet1)
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By(fmt.Sprintf("waiting for cluster to get back to original size. Final size should be %d nodes", initialClusterSize))
		waitForClusterSizeToBeHealthy(initialClusterSize)
	})
})

func waitForClusterSizeToBeHealthy(targetSize int) {
	client, err := e2e.LoadClient()
	o.Expect(err).NotTo(o.HaveOccurred())

	o.Eventually(func() int {
		err := machineSetsSnapShot(client)
		o.Expect(err).NotTo(o.HaveOccurred())

		finalClusterSize, err := getClusterSize(client)
		o.Expect(err).NotTo(o.HaveOccurred())
		return finalClusterSize
	}, e2e.WaitLong, 5*time.Second).Should(o.BeNumerically("==", targetSize))

	g.By(fmt.Sprintf("waiting for all nodes to be ready"))
	err = e2e.WaitUntilAllNodesAreReady(client)
	o.Expect(err).NotTo(o.HaveOccurred())

	g.By(fmt.Sprintf("waiting for all nodes to be schedulable"))
	err = waitUntilAllNodesAreSchedulable(client)
	o.Expect(err).NotTo(o.HaveOccurred())

	g.By(fmt.Sprintf("waiting for each node to be backed by a machine"))
	o.Expect(isOneMachinePerNode(client)).To(o.BeTrue())
}

// waitUntilAllNodesAreSchedulable waits for all cluster nodes to be schedulable and returns an error otherwise
func waitUntilAllNodesAreSchedulable(client runtimeclient.Client) error {
	return wait.PollImmediate(1*time.Second, time.Minute, func() (bool, error) {
		nodeList := corev1.NodeList{}
		if err := client.List(context.TODO(), &runtimeclient.ListOptions{}, &nodeList); err != nil {
			glog.Errorf("error querying api for nodeList object: %v, retrying...", err)
			return false, nil
		}
		// All nodes needs to be schedulable
		for _, node := range nodeList.Items {
			if node.Spec.Unschedulable == true {
				glog.Errorf("Node %q is unschedulable", node.Name)
				return false, nil
			}
			glog.Infof("Node %q is schedulable", node.Name)
		}
		return true, nil
	})
}
