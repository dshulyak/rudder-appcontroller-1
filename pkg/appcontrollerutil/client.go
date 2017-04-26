package appcontrollerutil

import (
	"bytes"
	"fmt"
	"io"
	"log"

	"google.golang.org/grpc/grpclog"

	"k8s.io/client-go/pkg/labels"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/runtime"

	"strings"

	"github.com/Mirantis/k8s-AppController/pkg/client"
	"github.com/Mirantis/k8s-AppController/pkg/report"
	"github.com/Mirantis/k8s-appcontroller/pkg/scheduler"
	"github.com/nebril/helm/pkg/kube"
)

type missingResource struct {
	name     string
	kind     string
	resource string
}

func (m *missingResource) Key() string {
	return fmt.Sprintf("%s/%s", strings.ToLower(m.kind), m.name)
}

func GetStatus(helmClient *kube.Client, namespace string, reader io.Reader) (string, error) {
	objs := make(map[string][]runtime.Object)
	infos, err := helmClient.BuildUnstructured(namespace, reader)
	if err != nil {
		return "", err
	}
	missing := []*missingResource{}
	err = perform(helmClient, namespace, infos, func(info *resource.Info) error {
		log.Printf("Doing get for %s: %q", info.Mapping.GroupVersionKind.Kind, info.Name)
		obj, err := resource.NewHelper(info.Client, info.Mapping).Get(info.Namespace, info.Name, info.Export)
		if err != nil {
			log.Printf("WARNING: Failed Get for resource %q: %s", info.Name, err)
			missing = append(missing, &missingResource{info.Name, info.GetObjectKind().GroupVersionKind().Kind, info.Mapping.Resource})
			return nil
		}
		// We need to grab the ObjectReference so we can correctly group the objects.
		or, err := api.GetReference(obj)
		if err != nil {
			log.Printf("FAILED GetReference for: %#v\n%v", obj, err)
			return err
		}

		// Use APIVersion/Kind as grouping mechanism. I'm not sure if you can have multiple
		// versions per cluster, but this certainly won't hurt anything, so let's be safe.
		objType := or.APIVersion + "/" + or.Kind
		objs[objType] = append(objs[objType], obj)
		return nil
	})
	if err != nil {
		return "", err
	}

	// Ok, now we have all the objects grouped by types (say, by v1/Pod, v1/Service, etc.), so
	// spin through them and print them. Printer is cool since it prints the header only when
	// an object type changes, so we can just rely on that. Problem is it doesn't seem to keep
	// track of tab widths
	buf := new(bytes.Buffer)
	p := kubectl.NewHumanReadablePrinter(kubectl.PrintOptions{})
	for t, ot := range objs {
		if _, err = buf.WriteString("==> " + t + "\n"); err != nil {
			return "", err
		}
		for _, o := range ot {
			if err := p.PrintObj(o, buf); err != nil {
				log.Printf("failed to print object type %s, object: %q :\n %v", t, o, err)
				return "", err
			}
		}
		if _, err := buf.WriteString("\n"); err != nil {
			return "", err
		}
	}
	if len(objs) == 0 {
		if _, err := buf.WriteString("\n"); err != nil {
			return "", err
		}
	}
	if len(missing) > 0 {
		namespacedClient, err := client.NewForNamespace("", namespace)
		if err != nil {
			return "", fmt.Errorf("couldn't create namespaced client. Err: %v", err)
		}
		// TODO set proper label helmRelease: blabla on resdef creation in rudder
		selector, err := labels.Parse("")
		if err != nil {
			return "", fmt.Errorf("could't parse release labels. Err: %v", err)
		}
		graph, err := scheduler.BuildDependencyGraph(namespacedClient, selector)
		if err != nil {
			return "", fmt.Errorf("couldn't create a dependency graph. Err: %v", err)
		}
		buf.WriteString("==> MISSING\nKIND\t\tNAME\t\tSTATUS\t\n")
		for _, m := range missing {
			grpclog.Printf("Looking for key %v in resource graph", m.Key())
			if scheduledResource, exist := graph[m.Key()]; exist {
				printMissingState(buf, m, scheduledResource.GetNodeReport(m.Key()))
			}
		}
	}
	return buf.String(), nil
}

func printMissingState(w io.Writer, m *missingResource, rep report.NodeReport) {
	fmt.Fprintf(w, "%s\t\t%s\t\t", m.resource, m.name)
	if rep.Blocked {
		fmt.Fprint(w, "WAITING_FOR:")
	} else {
		fmt.Fprint(w, "INPROGRESS")
	}
	for _, dep := range rep.Dependencies {
		if !dep.Blocks {
			continue
		}
		fmt.Fprintf(w, " %s,", dep.Dependency)
	}
	fmt.Fprint(w, "\t\n")
}

func perform(c *kube.Client, namespace string, infos kube.Result, fn kube.ResourceActorFunc) error {
	if len(infos) == 0 {
		return kube.ErrNoObjectsVisited
	}

	for _, info := range infos {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}
