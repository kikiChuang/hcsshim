package cpugroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kikiChuang/hcsshim/internal/hcs"
	hcsschema "github.com/kikiChuang/hcsshim/internal/schema2"
)

const NullGroupID = "00000000-0000-0000-0000-000000000000"

// ErrHVStatusInvalidCPUGroupState corresponds to the internal error code for HV_STATUS_INVALID_CPU_GROUP_STATE
var ErrHVStatusInvalidCPUGroupState = errors.New("The hypervisor could not perform the operation because the CPU group is entering or in an invalid state.")

// Delete deletes the cpugroup from the host
func Delete(ctx context.Context, id string) error {
	operation := hcsschema.DeleteGroup
	details := hcsschema.DeleteGroupOperation{
		GroupId: id,
	}

	return modifyCPUGroupRequest(ctx, operation, details)
}

// modifyCPUGroupRequest is a helper function for making modify calls to a cpugroup
func modifyCPUGroupRequest(ctx context.Context, operation hcsschema.CPUGroupOperation, details interface{}) error {
	req := hcsschema.ModificationRequest{
		PropertyType: hcsschema.PTCPUGroup,
		Settings: &hcsschema.HostProcessorModificationRequest{
			Operation:        operation,
			OperationDetails: details,
		},
	}

	return hcs.ModifyServiceSettings(ctx, req)
}

// Create creates a new cpugroup on the host with a prespecified id
func Create(ctx context.Context, id string, logicalProcessors []uint32) error {
	operation := hcsschema.CreateGroup
	details := &hcsschema.CreateGroupOperation{
		GroupId:               strings.ToLower(id),
		LogicalProcessors:     logicalProcessors,
		LogicalProcessorCount: uint32(len(logicalProcessors)),
	}
	if err := modifyCPUGroupRequest(ctx, operation, details); err != nil {
		return fmt.Errorf("failed to make cpugroups CreateGroup request for details %+v with: %s", details, err)
	}
	return nil
}

// getCPUGroupConfig finds the cpugroup config information for group with `id`
func getCPUGroupConfig(ctx context.Context, id string) (*hcsschema.CpuGroupConfig, error) {
	query := hcsschema.PropertyQuery{
		PropertyTypes: []hcsschema.PropertyType{hcsschema.PTCPUGroup},
	}
	cpuGroupsPresent, err := hcs.GetServiceProperties(ctx, query)
	if err != nil {
		return nil, err
	}
	groupConfigs := &hcsschema.CpuGroupConfigurations{}
	if err := json.Unmarshal(cpuGroupsPresent.Properties[0], groupConfigs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal host cpugroups: %v", err)
	}

	for _, c := range groupConfigs.CpuGroups {
		if strings.ToLower(c.GroupId) == strings.ToLower(id) {
			return &c, nil
		}
	}
	return nil, fmt.Errorf("no cpugroup exists with id %v", id)
}
