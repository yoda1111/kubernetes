/*
Copyright 2022 The Kubernetes Authors.

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

package validatingadmissionpolicy

import (
	"context"
	"fmt"
	"strings"

	celtypes "github.com/google/cel-go/common/types"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	apiservercel "k8s.io/apiserver/pkg/cel"
)

// validator implements the Validator interface
type validator struct {
	validationFilter      cel.Filter
	auditAnnotationFilter cel.Filter
	messageFilter         cel.Filter
	failPolicy            *v1.FailurePolicyType
	authorizer            authorizer.Authorizer
}

func NewValidator(validationFilter, auditAnnotationFilter, messageFilter cel.Filter, failPolicy *v1.FailurePolicyType, authorizer authorizer.Authorizer) Validator {
	return &validator{
		validationFilter:      validationFilter,
		auditAnnotationFilter: auditAnnotationFilter,
		messageFilter:         messageFilter,
		failPolicy:            failPolicy,
		authorizer:            authorizer,
	}
}

func policyDecisionActionForError(f v1.FailurePolicyType) PolicyDecisionAction {
	if f == v1.Ignore {
		return ActionAdmit
	}
	return ActionDeny
}

func auditAnnotationEvaluationForError(f v1.FailurePolicyType) PolicyAuditAnnotationAction {
	if f == v1.Ignore {
		return AuditAnnotationActionExclude
	}
	return AuditAnnotationActionError
}

// Validate takes a list of Evaluation and a failure policy and converts them into actionable PolicyDecisions
// runtimeCELCostBudget was added for testing purpose only. Callers should always use const RuntimeCELCostBudget from k8s.io/apiserver/pkg/apis/cel/config.go as input.
func (v *validator) Validate(ctx context.Context, versionedAttr *admission.VersionedAttributes, versionedParams runtime.Object, runtimeCELCostBudget int64) ValidateResult {
	var f v1.FailurePolicyType
	if v.failPolicy == nil {
		f = v1.Fail
	} else {
		f = *v.failPolicy
	}

	optionalVars := cel.OptionalVariableBindings{VersionedParams: versionedParams, Authorizer: v.authorizer}
	expressionOptionalVars := cel.OptionalVariableBindings{VersionedParams: versionedParams}
	admissionRequest := cel.CreateAdmissionRequest(versionedAttr.Attributes)
	evalResults, remainingBudget, err := v.validationFilter.ForInput(ctx, versionedAttr, admissionRequest, optionalVars, runtimeCELCostBudget)
	if err != nil {
		return ValidateResult{
			Decisions: []PolicyDecision{
				{
					Action:     policyDecisionActionForError(f),
					Evaluation: EvalError,
					Message:    err.Error(),
				},
			},
		}
	}
	decisions := make([]PolicyDecision, len(evalResults))
	messageResults, _, err := v.messageFilter.ForInput(ctx, versionedAttr, admissionRequest, expressionOptionalVars, remainingBudget)
	for i, evalResult := range evalResults {
		var decision = &decisions[i]
		// TODO: move this to generics
		validation, ok := evalResult.ExpressionAccessor.(*ValidationCondition)
		if !ok {
			klog.Error("Invalid type conversion to ValidationCondition")
			decision.Action = policyDecisionActionForError(f)
			decision.Evaluation = EvalError
			decision.Message = "Invalid type sent to validator, expected ValidationCondition"
			continue
		}

		var messageResult *cel.EvaluationResult
		var messageError *apiservercel.Error
		if len(messageResults) > i {
			messageResult = &messageResults[i]
		}
		messageError, _ = err.(*apiservercel.Error)
		if evalResult.Error != nil {
			decision.Action = policyDecisionActionForError(f)
			decision.Evaluation = EvalError
			decision.Message = evalResult.Error.Error()
		} else if messageError != nil &&
			(messageError.Type == apiservercel.ErrorTypeInternal ||
				(messageError.Type == apiservercel.ErrorTypeInvalid &&
					strings.HasPrefix(messageError.Detail, "validation failed due to running out of cost budget"))) {
			decision.Action = policyDecisionActionForError(f)
			decision.Evaluation = EvalError
			decision.Message = fmt.Sprintf("failed messageExpression: %s", err)
		} else if evalResult.EvalResult != celtypes.True {
			decision.Action = ActionDeny
			if validation.Reason == nil {
				decision.Reason = metav1.StatusReasonInvalid
			} else {
				decision.Reason = *validation.Reason
			}
			// decide the failure message
			var message string
			// attempt to set message with messageExpression result
			if messageResult != nil && messageResult.Error == nil && messageResult.EvalResult != nil {
				// also fallback if the eval result is non-string (including null) or
				// whitespaces.
				if message, ok = messageResult.EvalResult.Value().(string); ok {
					message = strings.TrimSpace(message)
					// deny excessively long message from EvalResult
					if len(message) > celconfig.MaxEvaluatedMessageExpressionSizeBytes {
						klog.V(2).InfoS("excessively long message denied", "message", message)
						message = ""
					}
					// deny message that contains newlines
					if strings.ContainsAny(message, "\n") {
						klog.V(2).InfoS("multi-line message denied", "message", message)
						message = ""
					}
				}
			}
			if messageResult != nil && messageResult.Error != nil {
				// log any error with messageExpression
				klog.V(2).ErrorS(messageResult.Error, "error while evaluating messageExpression")
			}
			// fallback to set message to the custom message
			if message == "" && len(validation.Message) > 0 {
				message = strings.TrimSpace(validation.Message)
			}
			// fallback to use the expression to compose a message
			if message == "" {
				message = fmt.Sprintf("failed expression: %v", strings.TrimSpace(validation.Expression))
			}
			decision.Message = message
		} else {
			decision.Action = ActionAdmit
			decision.Evaluation = EvalAdmit
		}
	}

	options := cel.OptionalVariableBindings{VersionedParams: versionedParams}
	auditAnnotationEvalResults, _, err := v.auditAnnotationFilter.ForInput(ctx, versionedAttr, cel.CreateAdmissionRequest(versionedAttr.Attributes), options, runtimeCELCostBudget)
	if err != nil {
		return ValidateResult{
			Decisions: []PolicyDecision{
				{
					Action:     policyDecisionActionForError(f),
					Evaluation: EvalError,
					Message:    err.Error(),
				},
			},
		}
	}

	auditAnnotationResults := make([]PolicyAuditAnnotation, len(auditAnnotationEvalResults))
	for i, evalResult := range auditAnnotationEvalResults {
		if evalResult.ExpressionAccessor == nil {
			continue
		}
		var auditAnnotationResult = &auditAnnotationResults[i]
		// TODO: move this to generics
		validation, ok := evalResult.ExpressionAccessor.(*AuditAnnotationCondition)
		if !ok {
			klog.Error("Invalid type conversion to AuditAnnotationCondition")
			auditAnnotationResult.Action = auditAnnotationEvaluationForError(f)
			auditAnnotationResult.Error = fmt.Sprintf("Invalid type sent to validator, expected AuditAnnotationCondition but got %T", evalResult.ExpressionAccessor)
			continue
		}
		auditAnnotationResult.Key = validation.Key

		if evalResult.Error != nil {
			auditAnnotationResult.Action = auditAnnotationEvaluationForError(f)
			auditAnnotationResult.Error = evalResult.Error.Error()
		} else {
			switch evalResult.EvalResult.Type() {
			case celtypes.StringType:
				value := strings.TrimSpace(evalResult.EvalResult.Value().(string))
				if len(value) == 0 {
					auditAnnotationResult.Action = AuditAnnotationActionExclude
				} else {
					auditAnnotationResult.Action = AuditAnnotationActionPublish
					auditAnnotationResult.Value = value
				}
			case celtypes.NullType:
				auditAnnotationResult.Action = AuditAnnotationActionExclude
			default:
				auditAnnotationResult.Action = AuditAnnotationActionError
				auditAnnotationResult.Error = fmt.Sprintf("valueExpression '%v' resulted in unsupported return type: %v. "+
					"Return type must be either string or null.", validation.ValueExpression, evalResult.EvalResult.Type())
			}
		}
	}
	return ValidateResult{Decisions: decisions, AuditAnnotations: auditAnnotationResults}
}
