/*
Copyright (c) 2021 Red Hat, Inc.

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

package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	cmv1 "github.com/openshift-online/ocm-sdk-go/clustersmgmt/v1"
	"github.com/openshift-online/ocm-sdk-go/errors"
	"github.com/openshift-online/ocm-sdk-go/logging"
)

type ClusterResourceType struct {
}

type ClusterResource struct {
	logger     logging.Logger
	collection *cmv1.ClustersClient
}

func (t *ClusterResourceType) GetSchema(ctx context.Context) (result tfsdk.Schema,
	diags diag.Diagnostics) {
	result = tfsdk.Schema{
		Description: "OpenShift managed cluster.",
		Attributes: map[string]tfsdk.Attribute{
			"id": {
				Description: "Unique identifier of the cluster.",
				Type:        types.StringType,
				Computed:    true,
			},
			"name": {
				Description: "Name of the cluster.",
				Type:        types.StringType,
				Required:    true,
			},
			"cloud_provider": {
				Description: "Cloud provider identifier, for example 'aws'.",
				Type:        types.StringType,
				Required:    true,
			},
			"cloud_region": {
				Description: "Cloud region identifier, for example 'us-east-1'.",
				Type:        types.StringType,
				Required:    true,
			},
			"multi_az": {
				Description: "Indicates if the cluster should be deployed to " +
					"multiple availability zones. Default value is 'false'.",
				Type:     types.BoolType,
				Optional: true,
				Computed: true,
				PlanModifiers: []tfsdk.AttributePlanModifier{
					tfsdk.RequiresReplace(),
				},
			},
			"properties": {
				Description: "User defined properties.",
				Type: types.MapType{
					ElemType: types.StringType,
				},
				Optional: true,
				Computed: true,
			},
			"api_url": {
				Description: "URL of the API server.",
				Type:        types.StringType,
				Computed:    true,
			},
			"console_url": {
				Description: "URL of the console.",
				Type:        types.StringType,
				Computed:    true,
			},
			"nodes": {
				Description: "Number and characteristis of nodes of the cluster.",
				Attributes:  t.nodesSchema(),
				Optional:    true,
			},
			"state": {
				Description: "State of the cluster.",
				Type:        types.StringType,
				Computed:    true,
			},
			"wait": {
				Description: "Wait till the cluster is ready.",
				Type:        types.BoolType,
				Optional:    true,
			},
		},
	}
	return
}

func (t *ClusterResourceType) nodesSchema() tfsdk.NestedAttributes {
	return tfsdk.SingleNestedAttributes(map[string]tfsdk.Attribute{
		"compute": {
			Description: "Number of compute nodes of the cluster.",
			Type:        types.Int64Type,
			Optional:    true,
			Computed:    true,
		},
		"compute_machine_type": {
			Description: "Identifier of the machine type used by the compute nodes, " +
				"for example `r5.xlarge`. Use the `ocm_machine_types` data " +
				"source to find the possible values.",
			Type:     types.StringType,
			Optional: true,
			Computed: true,
			PlanModifiers: []tfsdk.AttributePlanModifier{
				tfsdk.RequiresReplace(),
			},
		},
	})
}

func (t *ClusterResourceType) NewResource(ctx context.Context,
	p tfsdk.Provider) (result tfsdk.Resource, diags diag.Diagnostics) {
	// Cast the provider interface to the specific implementation:
	parent := p.(*Provider)

	// Get the collection of clusters:
	collection := parent.connection.ClustersMgmt().V1().Clusters()

	// Create the resource:
	result = &ClusterResource{
		logger:     parent.logger,
		collection: collection,
	}

	return
}

func (r *ClusterResource) Create(ctx context.Context,
	request tfsdk.CreateResourceRequest, response *tfsdk.CreateResourceResponse) {
	// Get the plan:
	state := &ClusterState{}
	diags := request.Plan.Get(ctx, state)
	response.Diagnostics.Append(diags...)
	if response.Diagnostics.HasError() {
		return
	}

	// Create the cluster:
	builder := cmv1.NewCluster()
	builder.Name(state.Name.Value)
	builder.CloudProvider(cmv1.NewCloudProvider().ID(state.CloudProvider.Value))
	builder.Region(cmv1.NewCloudRegion().ID(state.CloudRegion.Value))
	if !state.MultiAZ.Unknown && !state.MultiAZ.Null {
		builder.MultiAZ(state.MultiAZ.Value)
	}
	if !state.Properties.Unknown && !state.Properties.Null {
		properties := map[string]string{}
		for k, v := range state.Properties.Elems {
			properties[k] = v.(types.String).Value
		}
		builder.Properties(properties)
	}
	nodes := cmv1.NewClusterNodes()
	if !state.Nodes.Compute.Unknown && !state.Nodes.Compute.Null {
		nodes.Compute(int(state.Nodes.Compute.Value))
	}
	if !state.Nodes.ComputeMachineType.Unknown && !state.Nodes.ComputeMachineType.Null {
		nodes.ComputeMachineType(
			cmv1.NewMachineType().ID(state.Nodes.ComputeMachineType.Value),
		)
	}
	if !nodes.Empty() {
		builder.Nodes(nodes)
	}
	object, err := builder.Build()
	if err != nil {
		response.Diagnostics.AddError(
			"Can't build cluster",
			fmt.Sprintf(
				"Can't build cluster with name '%s': %v",
				state.Name.Value, err,
			),
		)
		return
	}
	add, err := r.collection.Add().Body(object).SendContext(ctx)
	if err != nil {
		response.Diagnostics.AddError(
			"Can't create cluster",
			fmt.Sprintf(
				"Can't create cluster with name '%s': %v",
				state.Name.Value, err,
			),
		)
		return
	}
	object = add.Body()

	// Wait till the cluster is ready unless explicitly disabled:
	wait := state.Wait.Unknown || state.Wait.Null || state.Wait.Value
	ready := object.State() == cmv1.ClusterStateReady
	if wait && !ready {
		pollCtx, cancel := context.WithTimeout(ctx, 1*time.Hour)
		defer cancel()
		_, err := r.collection.Cluster(object.ID()).Poll().
			Interval(30 * time.Second).
			Predicate(func(get *cmv1.ClusterGetResponse) bool {
				object = get.Body()
				return object.State() == cmv1.ClusterStateReady
			}).
			StartContext(pollCtx)
		if err != nil {
			response.Diagnostics.AddError(
				"Can't poll cluster state",
				fmt.Sprintf(
					"Can't poll state of cluster with identifier '%s': %v",
					object.ID(), err,
				),
			)
			return
		}
	}

	// Save the state:
	r.populateState(object, state)
	diags = response.State.Set(ctx, state)
	response.Diagnostics.Append(diags...)
}

func (r *ClusterResource) Read(ctx context.Context, request tfsdk.ReadResourceRequest,
	response *tfsdk.ReadResourceResponse) {
	// Get the current state:
	state := &ClusterState{}
	diags := request.State.Get(ctx, state)
	response.Diagnostics.Append(diags...)
	if response.Diagnostics.HasError() {
		return
	}

	// Find the cluster:
	get, err := r.collection.Cluster(state.ID.Value).Get().SendContext(ctx)
	if err != nil {
		response.Diagnostics.AddError(
			"Can't find cluster",
			fmt.Sprintf(
				"Can't find cluster with identifier '%s': %v",
				state.ID.Value, err,
			),
		)
		return
	}
	object := get.Body()

	// Save the state:
	r.populateState(object, state)
	diags = response.State.Set(ctx, state)
	response.Diagnostics.Append(diags...)
}

func (r *ClusterResource) Update(ctx context.Context, request tfsdk.UpdateResourceRequest,
	response *tfsdk.UpdateResourceResponse) {
	var diags diag.Diagnostics

	// Get the state:
	state := &ClusterState{}
	diags = request.State.Get(ctx, state)
	response.Diagnostics.Append(diags...)
	if response.Diagnostics.HasError() {
		return
	}

	// Get the plan:
	plan := &ClusterState{}
	diags = request.Plan.Get(ctx, plan)
	response.Diagnostics.Append(diags...)
	if response.Diagnostics.HasError() {
		return
	}

	// Send request to update the cluster:
	builder := cmv1.NewCluster()
	var nodes *cmv1.ClusterNodesBuilder
	compute, ok := shouldPatchInt(state.Nodes.Compute, plan.Nodes.Compute)
	if ok {
		nodes.Compute(int(compute))
	}
	if !nodes.Empty() {
		builder.Nodes(nodes)
	}
	patch, err := builder.Build()
	if err != nil {
		response.Diagnostics.AddError(
			"Can't build cluster patch",
			fmt.Sprintf(
				"Can't build patch for cluster with identifier '%s': %v",
				state.ID.Value, err,
			),
		)
		return
	}
	update, err := r.collection.Cluster(state.ID.Value).Update().
		Body(patch).
		SendContext(ctx)
	if err != nil {
		response.Diagnostics.AddError(
			"Can't update cluster",
			fmt.Sprintf(
				"Can't update cluster with identifier '%s': %v",
				state.ID.Value, err,
			),
		)
		return
	}
	object := update.Body()

	// Update the state:
	r.populateState(object, state)
	diags = response.State.Set(ctx, state)
	response.Diagnostics.Append(diags...)
}

func (r *ClusterResource) Delete(ctx context.Context, request tfsdk.DeleteResourceRequest,
	response *tfsdk.DeleteResourceResponse) {
	// Get the state:
	state := &ClusterState{}
	diags := request.State.Get(ctx, state)
	response.Diagnostics.Append(diags...)
	if response.Diagnostics.HasError() {
		return
	}

	// Send the request to delete the cluster:
	resource := r.collection.Cluster(state.ID.Value)
	_, err := resource.Delete().SendContext(ctx)
	if err != nil {
		response.Diagnostics.AddError(
			"Can't delete cluster",
			fmt.Sprintf(
				"Can't delete cluster with identifier '%s': %v",
				state.ID.Value, err,
			),
		)
		return
	}

	// Wait till the cluster has been effectively deleted:
	if state.Wait.Unknown || state.Wait.Null || state.Wait.Value {
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		_, err := resource.Poll().
			Interval(30 * time.Second).
			Status(http.StatusNotFound).
			StartContext(pollCtx)
		sdkErr, ok := err.(*errors.Error)
		if ok && sdkErr.Status() == http.StatusNotFound {
			err = nil
		}
		if err != nil {
			response.Diagnostics.AddError(
				"Can't poll cluster deletion",
				fmt.Sprintf(
					"Can't poll deletion of cluster with identifier '%s': %v",
					state.ID.Value, err,
				),
			)
			return
		}
	}

	// Remove the state:
	response.State.RemoveResource(ctx)
}

func (r *ClusterResource) ImportState(ctx context.Context, request tfsdk.ImportResourceStateRequest,
	response *tfsdk.ImportResourceStateResponse) {
	// Try to retrieve the object:
	get, err := r.collection.Cluster(request.ID).Get().SendContext(ctx)
	if err != nil {
		response.Diagnostics.AddError(
			"Can't find cluster",
			fmt.Sprintf(
				"Can't find cluster with identifier '%s': %v",
				request.ID, err,
			),
		)
		return
	}
	object := get.Body()

	// Save the state:
	state := &ClusterState{}
	r.populateState(object, state)
	diags := response.State.Set(ctx, state)
	response.Diagnostics.Append(diags...)
}

// populateState copies the data from the API object to the Terraform state.
func (r *ClusterResource) populateState(object *cmv1.Cluster, state *ClusterState) {
	state.ID = types.String{
		Value: object.ID(),
	}
	state.Name = types.String{
		Value: object.Name(),
	}
	state.CloudProvider = types.String{
		Value: object.CloudProvider().ID(),
	}
	state.CloudRegion = types.String{
		Value: object.Region().ID(),
	}
	state.MultiAZ = types.Bool{
		Value: object.MultiAZ(),
	}
	state.Properties = types.Map{
		ElemType: types.StringType,
		Elems:    map[string]attr.Value{},
	}
	for k, v := range object.Properties() {
		state.Properties.Elems[k] = types.String{
			Value: v,
		}
	}
	state.APIURL = types.String{
		Value: object.API().URL(),
	}
	state.ConsoleURL = types.String{
		Value: object.Console().URL(),
	}
	state.Nodes.Compute = types.Int64{
		Value: int64(object.Nodes().Compute()),
	}
	state.Nodes.ComputeMachineType = types.String{
		Value: object.Nodes().ComputeMachineType().ID(),
	}
	state.State = types.String{
		Value: string(object.State()),
	}
}