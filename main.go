package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type podRef struct {
	Name      string
	Namespace string
}

type check struct {
	Expires *time.Time
	Checks  []string
}

var checks = make(map[podRef]*check)

var client *kubernetes.Clientset
var m sync.RWMutex

func getClient(pathToCfg string) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error
	if pathToCfg == "" {
		log.Println("Using in-cluster config")
		config, err = rest.InClusterConfig()
	} else {
		log.Println("Using out-of-cluster config")
		config, err = clientcmd.BuildConfigFromFlags("", pathToCfg)
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

type resourceEventHandler struct {
	ev chan interface{}
}

func printObject(t string, obj interface{}) {
	switch obj := obj.(type) {
	case *v1.Pod:
		log.Println(t, "pod", obj.ObjectMeta.Namespace, obj.ObjectMeta.Name)
	}
}

func generateChecks(pod *v1.Pod) {
	// If one or more containers in the pod have an
	// HTTPS healthcheck, let's start tracking it.
	podCheck := check{
		Checks: make([]string, 0),
	}

	if pod.ObjectMeta.Namespace != "kube-system" {
		for _, cont := range pod.Spec.Containers {
			if cont.LivenessProbe != nil && cont.LivenessProbe.HTTPGet != nil {
				get := cont.LivenessProbe.HTTPGet

				if get.Scheme == v1.URISchemeHTTPS {
					port := get.Port.String()

					// If not an integer, let's get the port by name
					if _, err := strconv.Atoi(port); err != nil {
						for _, p := range cont.Ports {
							if p.Name == port {
								port = fmt.Sprintf("%d", p.ContainerPort)
								break
							}
						}
					}
					podCheck.Checks = append(podCheck.Checks, fmt.Sprintf("%s:%s", pod.Status.PodIP, port))
				}
			}
		}
	}

	if port, ok := pod.Annotations["cert-checker.zacharyseguin.ca/check-port"]; ok {
		podCheck.Checks = append(podCheck.Checks, fmt.Sprintf("%s:%s", pod.Status.PodIP, port))
	}

	p := podRef{Name: pod.ObjectMeta.Name, Namespace: pod.ObjectMeta.Namespace}
	if len(podCheck.Checks) > 0 {
		m.Lock()
		defer m.Unlock()

		checks[p] = &podCheck
	} else if _, ok := checks[p]; ok {
		m.Lock()
		defer m.Unlock()

		delete(checks, p)
	}
}

func (reh *resourceEventHandler) OnAdd(obj interface{}) {
	printObject("add", obj)

	switch obj := obj.(type) {
	case *v1.Pod:
		generateChecks(obj)
	}
}

func (reh *resourceEventHandler) OnUpdate(oldObj, newObj interface{}) {
	printObject("update", newObj)

	switch obj := newObj.(type) {
	case *v1.Pod:
		old := oldObj.(*v1.Pod)

		if obj.Status.Phase != old.Status.Phase ||
			obj.Status.PodIP != old.Status.PodIP ||
			obj.Annotations["cert-checker.zacharyseguin.ca/check-port"] != old.Annotations["cert-checker.zacharyseguin.ca/check-port"] ||
			len(obj.Spec.Containers) != len(old.Spec.Containers) {
			generateChecks(obj)
			return
		}

		for indx, cont := range obj.Spec.Containers {
			if cont.LivenessProbe != old.Spec.Containers[indx].LivenessProbe {
				generateChecks(obj)
				return
			}
		}
	}
}

func (reh *resourceEventHandler) OnDelete(obj interface{}) {
	switch obj := obj.(type) {
	case *v1.Pod:
		printObject("delete", obj)

		m.Lock()
		defer m.Unlock()
		p := podRef{Name: obj.ObjectMeta.Name, Namespace: obj.ObjectMeta.Namespace}
		delete(checks, p)
	}
}

func getExpiry(host string) (*time.Time, error) {
	log.Printf("Checking %s...", host)

	conn, err := tls.DialWithDialer(&net.Dialer{
		Timeout: time.Second * 20,
	}, "tcp", host, &tls.Config{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) > 0 {
		return &certs[0].NotAfter, nil
	}

	return nil, errors.New("No certs")
}

func expire() {
	for true {
		m.RLock()
		defer m.RUnlock()

		for pod, check := range checks {
			// Find the expiry time, if we don't have it already
			if check.Expires == nil {
				var expires *time.Time

				// Ensure the pod is ready before we check
				p, err := client.CoreV1().Pods(pod.Namespace).Get(pod.Name, metav1.GetOptions{})
				if err != nil {
					log.Printf("Unable to get pod %s/%s info", pod.Namespace, pod.Name)
					continue
				}

				ready := false
				for _, cond := range p.Status.Conditions {
					if cond.Type == v1.ContainersReady && cond.Status == v1.ConditionTrue {
						ready = true
						break
					}
				}

				if !ready {
					continue
				}

				// Run the checks, and save the lowest expiry time
				for _, host := range check.Checks {
					log.Println(pod, host)

					expiry, err := getExpiry(host)
					if err != nil {
						log.Println(err)
						continue
					}

					if expires == nil || expiry.Before(*expires) {
						expires = expiry
					}
				}

				// Try to randomize the removal in the 10 mins before expiry
				if expires != nil {
					delTime := expires.Add(-1 * (time.Second * time.Duration(rand.Intn(60*10))))
					check.Expires = &delTime

					log.Printf("Pod %s/%s expires at %s", pod.Namespace, pod.Name, *check.Expires)
				}
			}

			// If we have exceeded the expiry time, delete the pod.
			if check.Expires != nil && time.Now().After(*check.Expires) {
				log.Printf("Deleting pod %s/%s", pod.Namespace, pod.Name)
				client.CoreV1().Pods(pod.Namespace).Delete(pod.Name, nil)
			}
		}

		m.RUnlock()
		time.Sleep(time.Second)
	}
}

func main() {
	config := os.Getenv("KUBERNETES_SERVICE_HOST")
	if config == "" {
		config = "/Users/zachary/.kube/config"
	} else {
		config = ""
	}
	var err error
	client, err = getClient(config)
	if err != nil {
		log.Fatal(err)
	}

	// Updates
	log.Println("Subscribing for pod updates")

	eventHandler := &resourceEventHandler{
		ev: make(chan interface{}),
	}
	factory := informers.NewFilteredSharedInformerFactory(
		client,
		time.Minute*10,
		metav1.NamespaceAll,
		nil,
	)
	factory.Core().V1().Pods().Informer().AddEventHandler(eventHandler)

	stop := make(chan struct{})
	factory.Start(stop)
	for _, ok := range factory.WaitForCacheSync(nil) {
		if !ok {
			log.Fatal("Timed out waiting for controller caches to sync")
		}
	}

	go expire()

	<-stop
}
