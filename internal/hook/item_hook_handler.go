/*
Copyright 2020 the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package hook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"github.com/vmware-tanzu/velero/pkg/kuberesource"
	"github.com/vmware-tanzu/velero/pkg/podexec"
	"github.com/vmware-tanzu/velero/pkg/util/collections"
)

type hookPhase string

const (
	PhasePre  hookPhase = "pre"
	PhasePost hookPhase = "post"
)

// ItemHookHandler invokes hooks for an item.
type ItemHookHandler interface {
	// HandleHooks invokes hooks for an item. If the item is a pod and the appropriate annotations exist
	// to specify a hook, that is executed. Otherwise, this looks at the backup context's Backup to
	// determine if there are any hooks relevant to the item, taking into account the hook spec's
	// namespaces, resources, and label selector.
	HandleHooks(
		log logrus.FieldLogger,
		groupResource schema.GroupResource,
		obj runtime.Unstructured,
		resourceHooks []ResourceHook,
		phase hookPhase,
	) error
}

// DefaultItemHookHandler is the default itemHookHandler.
type DefaultItemHookHandler struct {
	PodCommandExecutor podexec.PodCommandExecutor
}

func (h *DefaultItemHookHandler) HandleHooks(
	log logrus.FieldLogger,
	groupResource schema.GroupResource,
	obj runtime.Unstructured,
	resourceHooks []ResourceHook,
	phase hookPhase,
) error {
	// We only support hooks on pods right now
	if groupResource != kuberesource.Pods {
		return nil
	}

	metadata, err := meta.Accessor(obj)
	if err != nil {
		return errors.Wrap(err, "unable to get a metadata accessor")
	}

	namespace := metadata.GetNamespace()
	name := metadata.GetName()

	// If the pod has the hook specified via annotations, that takes priority.
	hookFromAnnotations := getPodExecHookFromAnnotations(metadata.GetAnnotations(), phase, log)
	if phase == PhasePre && hookFromAnnotations == nil {
		// See if the pod has the legacy hook annotation keys (i.e. without a phase specified)
		hookFromAnnotations = getPodExecHookFromAnnotations(metadata.GetAnnotations(), "", log)
	}
	if hookFromAnnotations != nil {
		hookLog := log.WithFields(
			logrus.Fields{
				"hookSource": "annotation",
				"hookType":   "exec",
				"hookPhase":  phase,
			},
		)
		if err := h.PodCommandExecutor.ExecutePodCommand(hookLog, obj.UnstructuredContent(), namespace, name, "<from-annotation>", hookFromAnnotations); err != nil {
			hookLog.WithError(err).Error("Error executing hook")
			if hookFromAnnotations.OnError == api.HookErrorModeFail {
				return err
			}
		}

		return nil
	}

	labels := labels.Set(metadata.GetLabels())
	// Otherwise, check for hooks defined in the backup spec.
	for _, resourceHook := range resourceHooks {
		if !resourceHook.applicableTo(groupResource, namespace, labels) {
			continue
		}

		var hooks []api.BackupResourceHook
		if phase == PhasePre {
			hooks = resourceHook.Pre
		} else {
			hooks = resourceHook.Post
		}
		for _, hook := range hooks {
			if groupResource == kuberesource.Pods {
				if hook.Exec != nil {
					hookLog := log.WithFields(
						logrus.Fields{
							"hookSource": "backupSpec",
							"hookType":   "exec",
							"hookPhase":  phase,
						},
					)
					err := h.PodCommandExecutor.ExecutePodCommand(hookLog, obj.UnstructuredContent(), namespace, name, resourceHook.Name, hook.Exec)
					if err != nil {
						hookLog.WithError(err).Error("Error executing hook")
						if hook.Exec.OnError == api.HookErrorModeFail {
							return err
						}
					}
				}
			}
		}
	}

	return nil
}

const (
	podBackupHookContainerAnnotationKey = "hook.backup.velero.io/container"
	podBackupHookCommandAnnotationKey   = "hook.backup.velero.io/command"
	podBackupHookOnErrorAnnotationKey   = "hook.backup.velero.io/on-error"
	podBackupHookTimeoutAnnotationKey   = "hook.backup.velero.io/timeout"
)

func phasedKey(phase hookPhase, key string) string {
	if phase != "" {
		return fmt.Sprintf("%v.%v", phase, key)
	}
	return string(key)
}

func getHookAnnotation(annotations map[string]string, key string, phase hookPhase) string {
	return annotations[phasedKey(phase, key)]
}

// getPodExecHookFromAnnotations returns an ExecHook based on the annotations, as long as the
// 'command' annotation is present. If it is absent, this returns nil.
// If there is an error in parsing a supplied timeout, it is logged.
func getPodExecHookFromAnnotations(annotations map[string]string, phase hookPhase, log logrus.FieldLogger) *api.ExecHook {
	commandValue := getHookAnnotation(annotations, podBackupHookCommandAnnotationKey, phase)
	if commandValue == "" {
		return nil
	}
	var command []string
	// check for json array
	if commandValue[0] == '[' {
		if err := json.Unmarshal([]byte(commandValue), &command); err != nil {
			command = []string{commandValue}
		}
	} else {
		command = append(command, commandValue)
	}

	container := getHookAnnotation(annotations, podBackupHookContainerAnnotationKey, phase)

	onError := api.HookErrorMode(getHookAnnotation(annotations, podBackupHookOnErrorAnnotationKey, phase))
	if onError != api.HookErrorModeContinue && onError != api.HookErrorModeFail {
		onError = ""
	}

	var timeout time.Duration
	timeoutString := getHookAnnotation(annotations, podBackupHookTimeoutAnnotationKey, phase)
	if timeoutString != "" {
		if temp, err := time.ParseDuration(timeoutString); err == nil {
			timeout = temp
		} else {
			log.Warn(errors.Wrapf(err, "Unable to parse provided timeout %s, using default", timeoutString))
		}
	}

	return &api.ExecHook{
		Container: container,
		Command:   command,
		OnError:   onError,
		Timeout:   metav1.Duration{Duration: timeout},
	}
}

// ResourceHook is a hook for a given resource.
type ResourceHook struct {
	Name          string
	Namespaces    *collections.IncludesExcludes
	Resources     *collections.IncludesExcludes
	LabelSelector labels.Selector
	Pre           []api.BackupResourceHook
	Post          []api.BackupResourceHook
}

func (r ResourceHook) applicableTo(groupResource schema.GroupResource, namespace string, labels labels.Set) bool {
	if r.Namespaces != nil && !r.Namespaces.ShouldInclude(namespace) {
		return false
	}
	if r.Resources != nil && !r.Resources.ShouldInclude(groupResource.String()) {
		return false
	}
	if r.LabelSelector != nil && !r.LabelSelector.Matches(labels) {
		return false
	}
	return true
}
