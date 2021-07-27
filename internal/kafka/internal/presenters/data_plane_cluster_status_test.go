package presenters

import (
	"reflect"
	"testing"

	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/internal/api/private"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/api"
)

func TestConvertDataPlaneClusterStatus_AvailableStrimziVersions(t *testing.T) {
	tests := []struct {
		name                            string
		inputClusterUpdateStatusRequest func() *private.DataPlaneClusterUpdateStatusRequest
		want                            []api.StrimziVersion
		wantErr                         bool
	}{
		{
			name: "When setting a non empty ordered list of strimzi versions that list is stored as is",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.StrimziVersions = []string{"v1.0.0-0", "v2.0.0-0", "v3.0.0-0"}
				return request
			},
			want: []api.StrimziVersion{
				api.StrimziVersion{Version: "v1.0.0-0", Ready: true},
				api.StrimziVersion{Version: "v2.0.0-0", Ready: true},
				api.StrimziVersion{Version: "v3.0.0-0", Ready: true},
			},
			wantErr: false,
		},
		{
			name: "When setting a non empty unordered list of strimzi versions that list is stored in semver ascending order",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.StrimziVersions = []string{"v5.12.0-0", "v5.8.0-0", "v3.0.0-0"}
				return request
			},
			want: []api.StrimziVersion{
				api.StrimziVersion{Version: "v3.0.0-0", Ready: true},
				api.StrimziVersion{Version: "v5.8.0-0", Ready: true},
				api.StrimziVersion{Version: "v5.12.0-0", Ready: true},
			},
			wantErr: false,
		},
		{
			name: "When setting an empty list of strimzi versions that list is stored as the empty list",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.StrimziVersions = []string{}
				return request
			},
			want:    []api.StrimziVersion{},
			wantErr: false,
		},
		{
			name: "When setting a nil list of strimzi versions that list is stored as the empty list",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.StrimziVersions = nil
				return request
			},
			want:    []api.StrimziVersion{},
			wantErr: false,
		},
		{
			name: "When setting a non empty ordered list of strimzi it is stored as is",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.Strimzi = []private.DataPlaneClusterUpdateStatusRequestStrimzi{
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v1.0.0-0", Ready: true},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v2.0.0-0", Ready: false},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v3.0.0-0", Ready: true},
				}
				return request
			},
			want: []api.StrimziVersion{
				api.StrimziVersion{Version: "v1.0.0-0", Ready: true},
				api.StrimziVersion{Version: "v2.0.0-0", Ready: false},
				api.StrimziVersion{Version: "v3.0.0-0", Ready: true},
			},
			wantErr: false,
		},
		{
			name: "When setting a non empty unordered list of strimzi that list is stored in semver ascending order from the version attribute",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.Strimzi = []private.DataPlaneClusterUpdateStatusRequestStrimzi{
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v5.0.0-0", Ready: true},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v2.0.0-0", Ready: false},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v3.0.0-0", Ready: true},
				}
				return request
			},
			want: []api.StrimziVersion{
				api.StrimziVersion{Version: "v2.0.0-0", Ready: false},
				api.StrimziVersion{Version: "v3.0.0-0", Ready: true},
				api.StrimziVersion{Version: "v5.0.0-0", Ready: true},
			},
			wantErr: false,
		},
		{
			name: "When setting an empty list of strimzi that list is stored as the empty list",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.Strimzi = []private.DataPlaneClusterUpdateStatusRequestStrimzi{}
				return request
			},
			want:    []api.StrimziVersion{},
			wantErr: false,
		},
		{
			name: "When setting a nil list of strimzi that list is stored as the empty list",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.Strimzi = nil
				return request
			},
			want:    []api.StrimziVersion{},
			wantErr: false,
		},
		{
			name: "When setting both strimzi and strimziVersions then strimzi content takes precedence",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.Strimzi = []private.DataPlaneClusterUpdateStatusRequestStrimzi{
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v5.0.0-0", Ready: true},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v2.0.0-0", Ready: false},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v3.0.0-0", Ready: true},
				}
				request.StrimziVersions = []string{"v18.0.0-0", "v15.0.0-0", "v16.0.0-0"}
				return request
			},
			want: []api.StrimziVersion{
				api.StrimziVersion{Version: "v2.0.0-0", Ready: false},
				api.StrimziVersion{Version: "v3.0.0-0", Ready: true},
				api.StrimziVersion{Version: "v5.0.0-0", Ready: true},
			},
			wantErr: false,
		},
		{
			name: "When strimzi nor strimziVersions are defined an empty list is returned",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				return request
			},
			want:    []api.StrimziVersion{},
			wantErr: false,
		},
		{
			name: "When one of the strimzi versions does not follow the expected format an error is returned",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.StrimziVersions = []string{"v1invalid.0.0-0", "v2.0.0-0", "v3.0.0-0"}
				return request
			},
			want:    nil,
			wantErr: true,
		},
		{
			name: "When one of the versions in strimzi does not follow the expected format an error is returned",
			inputClusterUpdateStatusRequest: func() *private.DataPlaneClusterUpdateStatusRequest {
				request := sampleValidDataPlaneClusterUpdateStatusRequest()
				request.Strimzi = []private.DataPlaneClusterUpdateStatusRequestStrimzi{
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v1.0.0-0", Ready: true},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v2.0.0", Ready: false},
					private.DataPlaneClusterUpdateStatusRequestStrimzi{Version: "v3.0.0-0", Ready: true},
				}
				return request
			},
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputClusterStatusRequest := tt.inputClusterUpdateStatusRequest()
			res, err := ConvertDataPlaneClusterStatus(*inputClusterStatusRequest)
			gotErr := err != nil
			errResultTestFailed := false
			if !reflect.DeepEqual(gotErr, tt.wantErr) {
				errResultTestFailed = true
				t.Errorf("wantErr: %v got: %v", tt.wantErr, gotErr)
			}
			if !errResultTestFailed && !tt.wantErr {
				if !reflect.DeepEqual(res.AvailableStrimziVersions, tt.want) {
					t.Errorf("want: %v got: %v", tt.want, res)
				}
			}
		})
	}
}

func sampleValidDataPlaneClusterUpdateStatusRequest() *private.DataPlaneClusterUpdateStatusRequest {
	return &private.DataPlaneClusterUpdateStatusRequest{
		Conditions: []private.DataPlaneClusterUpdateStatusRequestConditions{
			{
				Type:   "Ready",
				Status: "True",
			},
		},
		Total: private.DataPlaneClusterUpdateStatusRequestTotal{
			IngressEgressThroughputPerSec: &[]string{"test"}[0],
			Connections:                   &[]int32{1000000}[0],
			DataRetentionSize:             &[]string{"test"}[0],
			Partitions:                    &[]int32{1000000}[0],
		},
		NodeInfo: &private.DataPlaneClusterUpdateStatusRequestNodeInfo{
			Ceiling:                &[]int32{20}[0],
			Floor:                  &[]int32{3}[0],
			Current:                &[]int32{5}[0],
			CurrentWorkLoadMinimum: &[]int32{3}[0],
		},
		Remaining: private.DataPlaneClusterUpdateStatusRequestTotal{
			Connections:                   &[]int32{1000000}[0],
			Partitions:                    &[]int32{1000000}[0],
			IngressEgressThroughputPerSec: &[]string{"test"}[0],
			DataRetentionSize:             &[]string{"test"}[0],
		},
		ResizeInfo: &private.DataPlaneClusterUpdateStatusRequestResizeInfo{
			NodeDelta: &[]int32{3}[0],
			Delta: &private.DataPlaneClusterUpdateStatusRequestResizeInfoDelta{
				Connections:                   &[]int32{10000}[0],
				Partitions:                    &[]int32{10000}[0],
				IngressEgressThroughputPerSec: &[]string{"test"}[0],
				DataRetentionSize:             &[]string{"test"}[0],
			},
		},
	}
}