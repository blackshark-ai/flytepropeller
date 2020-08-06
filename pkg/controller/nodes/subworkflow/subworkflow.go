package subworkflow

import (
	"context"
	"fmt"
	"strconv"

	"github.com/lyft/flytestdlib/logger"

	"github.com/lyft/flyteidl/gen/pb-go/flyteidl/core"
	"github.com/lyft/flytestdlib/storage"

	"github.com/lyft/flytepropeller/pkg/apis/flyteworkflow/v1alpha1"
	"github.com/lyft/flytepropeller/pkg/controller/executors"
	"github.com/lyft/flytepropeller/pkg/controller/nodes/errors"
	"github.com/lyft/flytepropeller/pkg/controller/nodes/handler"
)

// Subworkflow handler handles inline subWorkflows
type subworkflowHandler struct {
	nodeExecutor executors.Node
}

// Helper method that extracts the SubWorkflow from the ExecutionContext
func GetSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext) (v1alpha1.ExecutableSubWorkflow, error) {
	node := nCtx.Node()
	subID := *node.GetWorkflowNode().GetSubWorkflowRef()
	subWorkflow := nCtx.ExecutionContext().FindSubWorkflow(subID)
	if subWorkflow == nil {
		return nil, fmt.Errorf("failed to find sub workflow with ID [%s]", subID)
	}
	return subWorkflow, nil
}

// Performs an additional step of passing in and setting the inputs, before handling the execution of a SubWorkflow.
func (s *subworkflowHandler) startAndHandleSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext, subWorkflow v1alpha1.ExecutableSubWorkflow, nl executors.NodeLookup) (handler.Transition, error) {
	// Before starting the subworkflow, lets set the inputs for the Workflow. The inputs for a SubWorkflow are essentially
	// Copy of the inputs to the Node
	nodeInputs, err := nCtx.InputReader().Get(ctx)
	if err != nil {
		errMsg := fmt.Sprintf("Failed to read input. Error [%s]", err)
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.RuntimeExecutionError, errMsg, nil)), nil
	}

	startStatus, err := s.nodeExecutor.SetInputsForStartNode(ctx, nCtx.ExecutionContext(), subWorkflow, nl, nodeInputs)
	if err != nil {
		// NOTE: We are implicitly considering an error when setting the inputs as a system error and hence automatically retryable!
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoUndefined), err
	}

	if startStatus.HasFailed() {
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailureErr(startStatus.Err, nil)), nil
	}

	return s.handleSubWorkflow(ctx, nCtx, subWorkflow, nl)
}

// Calls the recursive node executor to handle the SubWorkflow and translates the results after the success
func (s *subworkflowHandler) handleSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext, subworkflow v1alpha1.ExecutableSubWorkflow, nl executors.NodeLookup) (handler.Transition, error) {

	state, err := s.nodeExecutor.RecursiveNodeHandler(ctx, nCtx.ExecutionContext(), subworkflow, nl, subworkflow.StartNode())
	if err != nil {
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoUndefined), err
	}

	if state.HasFailed() {
		workflowNodeState := handler.WorkflowNodeState{
			Phase: v1alpha1.WorkflowNodePhaseFailing,
			Error: state.Err,
		}

		err = nCtx.NodeStateWriter().PutWorkflowNodeState(workflowNodeState)
		if subworkflow.GetOnFailureNode() != nil {
			return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailingErr(state.Err, nil)), err
		}

		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailureErr(state.Err, nil)), err
	}

	if state.IsComplete() {
		// If the WF interface has outputs, validate that the outputs file was written.
		var oInfo *handler.OutputInfo
		if outputBindings := subworkflow.GetOutputBindings(); len(outputBindings) > 0 {
			endNodeStatus, err := nl.GetNodeExecutionStatus(ctx, v1alpha1.EndNodeID)
			if err != nil {
				return handler.UnknownTransition, err
			}
			store := nCtx.DataStore()
			if endNodeStatus == nil {
				return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.SubWorkflowExecutionFailed, "No end node found in subworkflow.", nil)), err
			}

			sourcePath := v1alpha1.GetOutputsFile(endNodeStatus.GetOutputDir())
			if metadata, err := store.Head(ctx, sourcePath); err == nil {
				if !metadata.Exists() {
					errMsg := fmt.Sprintf("Subworkflow is expected to produce outputs but no outputs file was written to %v.", sourcePath)
					return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.SubWorkflowExecutionFailed, errMsg, nil)), nil
				}
			} else {
				return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoUndefined), nil
			}

			// TODO optimization, we could just point the outputInfo to the path of the subworkflows output
			destinationPath := v1alpha1.GetOutputsFile(nCtx.NodeStatus().GetOutputDir())
			if err := store.CopyRaw(ctx, sourcePath, destinationPath, storage.Options{}); err != nil {
				errMsg := fmt.Sprintf("Failed to copy subworkflow outputs from [%v] to [%v]", sourcePath, destinationPath)
				return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.SubWorkflowExecutionFailed, errMsg, nil)), nil
			}
			oInfo = &handler.OutputInfo{OutputURI: destinationPath}
		}

		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoSuccess(&handler.ExecutionInfo{
			OutputInfo: oInfo,
		})), nil
	}

	if state.PartiallyComplete() {
		if err := nCtx.EnqueueOwnerFunc()(); err != nil {
			return handler.UnknownTransition, err
		}
	}

	return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoRunning(nil)), nil
}

func (s *subworkflowHandler) HandleFailureNodeOfSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext, subworkflow v1alpha1.ExecutableSubWorkflow, nl executors.NodeLookup) (handler.Transition, error) {
	originalError := nCtx.NodeStateReader().GetWorkflowNodeState().Error
	if subworkflow.GetOnFailureNode() != nil {
		state, err := s.nodeExecutor.RecursiveNodeHandler(ctx, nCtx.ExecutionContext(), subworkflow, nl, subworkflow.GetOnFailureNode())
		if err != nil {
			return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoUndefined), err
		}

		if state.NodePhase == executors.NodePhaseRunning {
			return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoRunning(nil)), nil
		}

		if state.HasFailed() {
			// If handling failure node resulted in failure, its failure will mask the original failure for the workflow
			// TODO: Consider returning both errors.
			return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailureErr(state.Err, nil)), nil
		}

		if state.PartiallyComplete() {
			if err := nCtx.EnqueueOwnerFunc()(); err != nil {
				return handler.UnknownTransition, err
			}

			return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailingErr(originalError, nil)), nil
		}

		// When handling the failure node succeeds, the final status will still be failure
		// we use the original error as the failure reason.
		if state.IsComplete() {
			return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailureErr(
				originalError, nil)), nil
		}
	}

	return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailureErr(
		originalError, nil)), nil
}

func (s *subworkflowHandler) HandleFailingSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext) (handler.Transition, error) {
	subWorkflow, err := GetSubWorkflow(ctx, nCtx)
	if err != nil {
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.SubWorkflowExecutionFailed, err.Error(), nil)), nil
	}

	status := nCtx.NodeStatus()
	if subWorkflow.GetOnFailureNode() == nil {
		logger.Infof(ctx, "Subworkflow has no failure nodes, failing immediately.")
		return handler.DoTransition(handler.TransitionTypeEphemeral,
			handler.PhaseInfoFailureErr(nCtx.NodeStateReader().GetWorkflowNodeState().Error, nil)), err
	}

	nodeLookup := executors.NewNodeLookup(subWorkflow, status)
	return s.HandleFailureNodeOfSubWorkflow(ctx, nCtx, subWorkflow, nodeLookup)
}

func (s *subworkflowHandler) fetchNodeLookupForSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext, subWorkflow v1alpha1.ExecutableSubWorkflow) (executors.NodeLookup, error) {
	status := nCtx.NodeStatus()
	uniqueparentID := status.GetUniqueNodeID()
	currentAttemptStr := strconv.Itoa(int(nCtx.CurrentAttempt()))

	// We are only adding new fields here. Existing ones will remain as it is.
	for _, n := range subWorkflow.GetNodes() {
		subWorkflowNodeStatus, err := status.GetNodeExecutionStatus(ctx, n)
		if err != nil {
			return nil, err
		}
		newID, err := v1alpha1.ComputeUniqueIDForNode(n, *uniqueparentID, currentAttemptStr)
		if err != nil {
			return nil, err
		}
		subWorkflowNodeStatus.SetUniqueParentNodeID(status.GetUniqueNodeID())
		subWorkflowNodeStatus.SetUniqueNodeID(&newID)
		// This is used for grouping child nodes based on attempts
		subWorkflowNodeStatus.SetParentAttempts(nCtx.CurrentAttempt())
	}
	return executors.NewNodeLookup(subWorkflow, status), nil
}

func (s *subworkflowHandler) StartSubWorkflow(ctx context.Context, nCtx handler.NodeExecutionContext) (handler.Transition, error) {
	subWorkflow, err := GetSubWorkflow(ctx, nCtx)
	if err != nil {
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.SubWorkflowExecutionFailed, err.Error(), nil)), nil
	}

	nodeLookup, err := s.fetchNodeLookupForSubWorkflow(ctx, nCtx, subWorkflow)
	if err != nil {
		return handler.UnknownTransition, err
	}
	// assert startStatus.IsComplete() == true
	return s.startAndHandleSubWorkflow(ctx, nCtx, subWorkflow, nodeLookup)
}

func (s *subworkflowHandler) CheckSubWorkflowStatus(ctx context.Context, nCtx handler.NodeExecutionContext) (handler.Transition, error) {
	subWorkflow, err := GetSubWorkflow(ctx, nCtx)
	if err != nil {
		return handler.DoTransition(handler.TransitionTypeEphemeral, handler.PhaseInfoFailure(core.ExecutionError_SYSTEM, errors.SubWorkflowExecutionFailed, err.Error(), nil)), nil
	}

	nodeLookup, err := s.fetchNodeLookupForSubWorkflow(ctx, nCtx, subWorkflow)
	if err != nil {
		return handler.UnknownTransition, err
	}
	return s.startAndHandleSubWorkflow(ctx, nCtx, subWorkflow, nodeLookup)
}

func (s *subworkflowHandler) HandleAbort(ctx context.Context, nCtx handler.NodeExecutionContext, reason string) error {
	subWorkflow, err := GetSubWorkflow(ctx, nCtx)
	if err != nil {
		return err
	}
	status := nCtx.NodeStatus()
	nodeLookup := executors.NewNodeLookup(subWorkflow, status)
	return s.nodeExecutor.AbortHandler(ctx, nCtx.ExecutionContext(), subWorkflow, nodeLookup, subWorkflow.StartNode(), reason)
}

func newSubworkflowHandler(nodeExecutor executors.Node) subworkflowHandler {
	return subworkflowHandler{
		nodeExecutor: nodeExecutor,
	}
}
