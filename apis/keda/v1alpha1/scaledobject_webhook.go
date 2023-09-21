/*
Copyright 2023 The KEDA Authors

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

package v1alpha1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	"github.com/antonmedv/expr"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	prommetrics "github.com/kedacore/keda/v2/pkg/prommetrics/webhook"
)

var scaledobjectlog = logf.Log.WithName("scaledobject-validation-webhook")

var kc client.Client
var restMapper meta.RESTMapper

var memoryString = "memory"
var cpuString = "cpu"

func (so *ScaledObject) SetupWebhookWithManager(mgr ctrl.Manager) error {
	kc = mgr.GetClient()
	restMapper = mgr.GetRESTMapper()
	return ctrl.NewWebhookManagedBy(mgr).
		For(so).
		Complete()
}

// +kubebuilder:webhook:path=/validate-keda-sh-v1alpha1-scaledobject,mutating=false,failurePolicy=ignore,sideEffects=None,groups=keda.sh,resources=scaledobjects,verbs=create;update,versions=v1alpha1,name=vscaledobject.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &ScaledObject{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (so *ScaledObject) ValidateCreate() (admission.Warnings, error) {
	val, _ := json.MarshalIndent(so, "", "  ")
	scaledobjectlog.V(1).Info(fmt.Sprintf("validating scaledobject creation for %s", string(val)))
	return validateWorkload(so, "create")
}

func (so *ScaledObject) ValidateUpdate(old runtime.Object) (admission.Warnings, error) {
	val, _ := json.MarshalIndent(so, "", "  ")
	scaledobjectlog.V(1).Info(fmt.Sprintf("validating scaledobject update for %s", string(val)))

	if isRemovingFinalizer(so, old) {
		scaledobjectlog.V(1).Info("finalizer removal, skipping validation")
		return nil, nil
	}

	return validateWorkload(so, "update")
}

func (so *ScaledObject) ValidateDelete() (admission.Warnings, error) {
	return nil, nil
}

func isRemovingFinalizer(so *ScaledObject, old runtime.Object) bool {
	oldSo := old.(*ScaledObject)

	soSpec, _ := json.MarshalIndent(so.Spec, "", "  ")
	oldSoSpec, _ := json.MarshalIndent(oldSo.Spec, "", "  ")
	soSpecString := string(soSpec)
	oldSoSpecString := string(oldSoSpec)

	return len(so.ObjectMeta.Finalizers) == 0 && len(oldSo.ObjectMeta.Finalizers) == 1 && soSpecString == oldSoSpecString
}

func validateWorkload(so *ScaledObject, action string) (admission.Warnings, error) {
	prommetrics.RecordScaledObjectValidatingTotal(so.Namespace, action)

	verifyFunctions := []func(*ScaledObject, string) error{
		verifyCPUMemoryScalers,
		verifyTriggers,
		verifyScaledObjects,
		verifyHpas,
	}

	for i := range verifyFunctions {
		err := verifyFunctions[i](so, action)
		if err != nil {
			return nil, err
		}
	}

	scaledobjectlog.V(1).Info(fmt.Sprintf("scaledobject %s is valid", so.Name))
	return nil, nil
}

func verifyTriggers(incomingSo *ScaledObject, action string) error {
	err := ValidateTriggers(incomingSo.Spec.Triggers)
	if err != nil {
		scaledobjectlog.WithValues("name", incomingSo.Name).Error(err, "validation error")
		prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "incorrect-triggers")
	}
	return err
}

func verifyHpas(incomingSo *ScaledObject, action string) error {
	hpaList := &autoscalingv2.HorizontalPodAutoscalerList{}
	opt := &client.ListOptions{
		Namespace: incomingSo.Namespace,
	}
	err := kc.List(context.Background(), hpaList, opt)
	if err != nil {
		return err
	}

	var incomingSoGckr GroupVersionKindResource
	incomingSoGckr, err = ParseGVKR(restMapper, incomingSo.Spec.ScaleTargetRef.APIVersion, incomingSo.Spec.ScaleTargetRef.Kind)
	if err != nil {
		scaledobjectlog.Error(err, "Failed to parse Group, Version, Kind, Resource from incoming ScaledObject", "apiVersion", incomingSo.Spec.ScaleTargetRef.APIVersion, "kind", incomingSo.Spec.ScaleTargetRef.Kind)
		return err
	}

	for _, hpa := range hpaList.Items {
		val, _ := json.MarshalIndent(hpa, "", "  ")
		scaledobjectlog.V(1).Info(fmt.Sprintf("checking hpa %s: %v", hpa.Name, string(val)))

		hpaGckr, err := ParseGVKR(restMapper, hpa.Spec.ScaleTargetRef.APIVersion, hpa.Spec.ScaleTargetRef.Kind)
		if err != nil {
			scaledobjectlog.Error(err, "Failed to parse Group, Version, Kind, Resource from HPA", "hpaName", hpa.Name, "apiVersion", hpa.Spec.ScaleTargetRef.APIVersion, "kind", hpa.Spec.ScaleTargetRef.Kind)
			return err
		}

		if hpaGckr.GVKString() == incomingSoGckr.GVKString() &&
			hpa.Spec.ScaleTargetRef.Name == incomingSo.Spec.ScaleTargetRef.Name {
			owned := false
			for _, owner := range hpa.OwnerReferences {
				if owner.Kind == incomingSo.Kind {
					if owner.Name == incomingSo.Name {
						owned = true
						break
					}
				}
			}

			if !owned {
				if incomingSo.ObjectMeta.Annotations[ScaledObjectTransferHpaOwnershipAnnotation] == "true" &&
					incomingSo.Spec.Advanced.HorizontalPodAutoscalerConfig.Name == hpa.Name {
					scaledobjectlog.Info(fmt.Sprintf("%s hpa ownership being transferred to %s", hpa.Name, incomingSo.Name))
				} else {
					err = fmt.Errorf("the workload '%s' of type '%s' is already managed by the hpa '%s'", incomingSo.Spec.ScaleTargetRef.Name, incomingSoGckr.GVKString(), hpa.Name)
					scaledobjectlog.Error(err, "validation error")
					prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "other-hpa")
					return err
				}
			}
		}
	}
	return nil
}

func verifyScaledObjects(incomingSo *ScaledObject, action string) error {
	soList := &ScaledObjectList{}
	opt := &client.ListOptions{
		Namespace: incomingSo.Namespace,
	}
	err := kc.List(context.Background(), soList, opt)
	if err != nil {
		return err
	}

	incomingSoGckr, err := ParseGVKR(restMapper, incomingSo.Spec.ScaleTargetRef.APIVersion, incomingSo.Spec.ScaleTargetRef.Kind)
	if err != nil {
		scaledobjectlog.Error(err, "Failed to parse Group, Version, Kind, Resource from incoming ScaledObject", "apiVersion", incomingSo.Spec.ScaleTargetRef.APIVersion, "kind", incomingSo.Spec.ScaleTargetRef.Kind)
		return err
	}

	for _, so := range soList.Items {
		if so.Name == incomingSo.Name {
			continue
		}
		val, _ := json.MarshalIndent(so, "", "  ")
		scaledobjectlog.V(1).Info(fmt.Sprintf("checking scaledobject %s: %v", so.Name, string(val)))

		soGckr, err := ParseGVKR(restMapper, so.Spec.ScaleTargetRef.APIVersion, so.Spec.ScaleTargetRef.Kind)
		if err != nil {
			scaledobjectlog.Error(err, "Failed to parse Group, Version, Kind, Resource from ScaledObject", "soName", so.Name, "apiVersion", so.Spec.ScaleTargetRef.APIVersion, "kind", so.Spec.ScaleTargetRef.Kind)
			return err
		}

		if soGckr.GVKString() == incomingSoGckr.GVKString() &&
			so.Spec.ScaleTargetRef.Name == incomingSo.Spec.ScaleTargetRef.Name {
			err = fmt.Errorf("the workload '%s' of type '%s' is already managed by the ScaledObject '%s'", so.Spec.ScaleTargetRef.Name, incomingSoGckr.GVKString(), so.Name)
			scaledobjectlog.Error(err, "validation error")
			prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "other-scaled-object")
			return err
		}
	}

	// verify ScalingModifiers structure if defined in ScaledObject
	if incomingSo.Spec.Advanced != nil && !reflect.DeepEqual(incomingSo.Spec.Advanced.ScalingModifiers, ScalingModifiers{}) {
		_, _, err = ValidateScalingModifiers(incomingSo, []autoscalingv2.MetricSpec{}, "webhook")
		if err != nil {
			scaledobjectlog.Error(err, "error validating ScalingModifiers")
			prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "complex-scaling-logic")

			return err
		}
	}
	return nil
}

func verifyCPUMemoryScalers(incomingSo *ScaledObject, action string) error {
	var podSpec *corev1.PodSpec
	for _, trigger := range incomingSo.Spec.Triggers {
		if trigger.Type == cpuString || trigger.Type == memoryString {
			if podSpec == nil {
				key := types.NamespacedName{
					Namespace: incomingSo.Namespace,
					Name:      incomingSo.Spec.ScaleTargetRef.Name,
				}
				incomingSoGckr, err := ParseGVKR(restMapper, incomingSo.Spec.ScaleTargetRef.APIVersion, incomingSo.Spec.ScaleTargetRef.Kind)
				if err != nil {
					scaledobjectlog.Error(err, "Failed to parse Group, Version, Kind, Resource from incoming ScaledObject", "apiVersion", incomingSo.Spec.ScaleTargetRef.APIVersion, "kind", incomingSo.Spec.ScaleTargetRef.Kind)
					return err
				}

				switch incomingSoGckr.GVKString() {
				case "apps/v1.Deployment":
					deployment := &appsv1.Deployment{}
					err := kc.Get(context.Background(), key, deployment, &client.GetOptions{})
					if err != nil {
						return err
					}
					podSpec = &deployment.Spec.Template.Spec
				case "apps/v1.StatefulSet":
					statefulset := &appsv1.StatefulSet{}
					err := kc.Get(context.Background(), key, statefulset, &client.GetOptions{})
					if err != nil {
						return err
					}
					podSpec = &statefulset.Spec.Template.Spec
				default:
					return nil
				}
			}
			conainerName := trigger.Metadata["containerName"]
			for _, container := range podSpec.Containers {
				if conainerName != "" && container.Name != conainerName {
					continue
				}
				if trigger.Type == cpuString {
					if container.Resources.Requests == nil ||
						container.Resources.Requests.Cpu() == nil ||
						container.Resources.Requests.Cpu().AsApproximateFloat64() == 0 {
						err := fmt.Errorf("the scaledobject has a cpu trigger but the container %s doesn't have the cpu request defined", container.Name)
						scaledobjectlog.Error(err, "validation error")
						prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "missing-requests")
						return err
					}
				} else if trigger.Type == memoryString {
					if container.Resources.Requests == nil ||
						container.Resources.Requests.Memory() == nil ||
						container.Resources.Requests.Memory().AsApproximateFloat64() == 0 {
						err := fmt.Errorf("the scaledobject has a memory trigger but the container %s doesn't have the memory request defined", container.Name)
						scaledobjectlog.Error(err, "validation error")
						prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "missing-requests")
						return err
					}
				}
			}

			// validate scaledObject with cpu/mem triggers:
			// If scaled object has only cpu/mem triggers AND has minReplicaCount 0
			// return an error because it will never scale to zero
			scaleToZeroErr := true
			for _, trig := range incomingSo.Spec.Triggers {
				if trig.Type != cpuString && trig.Type != memoryString {
					scaleToZeroErr = false
					break
				}
			}

			if (scaleToZeroErr && incomingSo.Spec.MinReplicaCount == nil) || (scaleToZeroErr && *incomingSo.Spec.MinReplicaCount == 0) {
				err := fmt.Errorf("scaledobject has only cpu/memory triggers AND minReplica is 0 (scale to zero doesn't work in this case)")
				scaledobjectlog.Error(err, "validation error")
				prommetrics.RecordScaledObjectValidatingErrors(incomingSo.Namespace, action, "scale-to-zero-requirements-not-met")
				return err
			}
		}
	}
	return nil
}

// ValidateScalingModifiers validates all combinations of given arguments
// and their values
func ValidateScalingModifiers(so *ScaledObject, specs []autoscalingv2.MetricSpec, identifier string) (float64, autoscalingv2.MetricTargetType, error) {
	sm := so.Spec.Advanced.ScalingModifiers

	if sm.Formula == "" {
		return -1, autoscalingv2.MetricTargetType(""), fmt.Errorf("error ScalingModifiers.Formula is mandatory")
	}

	var num float64
	var metricType autoscalingv2.MetricTargetType

	// validate formula if not empty
	if err := validateScalingModifiersFormula(so, identifier); err != nil {
		err := errors.Join(fmt.Errorf("error validating formula in ScalingModifiers"), err)
		return -1, autoscalingv2.MetricTargetType(""), err
	}
	// validate target if not empty
	num, metricType, err := validateScalingModifiersTarget(so, specs)
	if err != nil {
		err := errors.Join(fmt.Errorf("error validating target in ScalingModifiers"), err)
		return -1, autoscalingv2.MetricTargetType(""), err
	}
	return num, metricType, nil
}

func validateScalingModifiersFormula(so *ScaledObject, identifier string) error {
	sm := so.Spec.Advanced.ScalingModifiers

	// if formula is empty, nothing to validate
	if sm.Formula == "" {
		return nil
	}
	// formula needs target because it's always transformed to Composite scaler
	if sm.Target == "" {
		return fmt.Errorf("formula is given but target is empty")
	}

	// Compile & Run with dummy values to determine if all triggers in formula are
	// defined (have names) ONLY in Webhook to avoid compiling in reconcile loop
	if identifier == "webhook" {
		triggersMap := make(map[string]float64)
		for _, trig := range so.Spec.Triggers {
			// if resource metrics are given, skip
			if trig.Type == cpuString || trig.Type == memoryString {
				continue
			}
			if trig.Name != "" {
				triggersMap[trig.Name] = 1.0
			}
		}
		compiled, err := expr.Compile(sm.Formula, expr.Env(triggersMap), expr.AsFloat64())
		if err != nil {
			return err
		}
		_, err = expr.Run(compiled, triggersMap)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateScalingModifiersTarget(so *ScaledObject, specs []autoscalingv2.MetricSpec) (float64, autoscalingv2.MetricTargetType, error) {
	sm := so.Spec.Advanced.ScalingModifiers

	if sm.Target == "" {
		return -1, "", nil
	}
	// convert string to float
	num, err := strconv.ParseFloat(sm.Target, 64)
	if err != nil || num <= 0.0 {
		return -1, "", fmt.Errorf("error converting target for complex logic (string->float) to valid target: %w", err)
	}

	// if target is given, composite-scaler will be passed to HPA -> all types
	// need to be the same make sure all metrics are of the same metricTargetType

	// check trigger types in SO
	var trigType autoscalingv2.MetricTargetType

	// gauron99: possible TODO: more sofisticated check for trigger could be used here
	// as well if solution is found (check just the right triggers that are used)
	for _, trig := range so.Spec.Triggers {
		if trig.Type == cpuString || trig.Type == memoryString || trig.Name == "" {
			continue
		}
		var current autoscalingv2.MetricTargetType
		if trig.MetricType == "" {
			current = autoscalingv2.AverageValueMetricType // default is AverageValue
		} else {
			current = trig.MetricType
		}
		if trigType == "" {
			trigType = current
		} else if trigType != current {
			err := fmt.Errorf("error trigger types are not the same for composite scaler: %s & %s", trigType, current)
			return -1, "", err
		}
	}
	if trigType == autoscalingv2.UtilizationMetricType {
		err := fmt.Errorf("error trigger type is Utilization, but it needs to be AverageValue or Value for external metrics")
		return -1, "", err
	}

	// check metric specs
	var metricType autoscalingv2.MetricTargetType
	for _, metric := range specs {
		if metric.External == nil {
			continue
		}
		if metricType == "" {
			metricType = metric.External.Target.Type
		} else if metric.External.Target.Type != metricType {
			err := fmt.Errorf("error metric target type is not the same for composite scaler: %s & %s", metricType, metric.External.Target.Type)
			return -1, "", err
		}
	}
	if metricType == autoscalingv2.UtilizationMetricType {
		err := fmt.Errorf("error metric target type is Utilization, but it needs to be AverageValue or Value for external metrics")
		return -1, "", err
	}
	return num, metricType, nil
}
