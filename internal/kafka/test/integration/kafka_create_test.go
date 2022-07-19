package integration

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/constants"
	constants2 "github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/constants"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/internal/api/dbapi"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/internal/api/public"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/internal/config"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/internal/kafkas/types"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/internal/presenters"
	kafkatest "github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/test"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/test/common"
	mockkafkas "github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/test/mocks/kafkas"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/kafka/test/mocks/kasfleetshardsync"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/api"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/metrics"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/test/mocks"

	"github.com/onsi/gomega"
)

// Test Kafka creation with manual scaling enabled
func TestKafkaCreate_ManualScaling(t *testing.T) {
	g := gomega.NewWithT(t)

	ocmServer := mocks.NewMockConfigurableServerBuilder().Build()
	defer ocmServer.Close()

	var enableManualScaling bool
	clusterList := config.ClusterList{}
	h, client, teardown := kafkatest.NewKafkaHelperWithHooks(t, ocmServer, func(d *config.DataplaneClusterConfig) {
		if enableManualScaling {
			// Enable manual scaling and register data plane cluster to cluster config
			d.DataPlaneClusterScalingType = config.ManualScaling
			d.ClusterConfig = config.NewClusterConfig(clusterList)
		}
	})
	defer teardown()

	// run mock fleetshard sync
	mockKasFleetshardSyncBuilder := kasfleetshardsync.NewMockKasFleetshardSyncBuilder(h, t)
	mockKasFleetshardSync := mockKasFleetshardSyncBuilder.Build()
	mockKasFleetshardSync.Start()
	defer mockKasFleetshardSync.Stop()

	// set up data plane cluster
	clusterID, getClusterErr := common.GetOSDClusterIDAndWaitForStatus(h, t, api.ClusterReady)
	g.Expect(getClusterErr).ToNot(gomega.HaveOccurred(), "failed to get dataplane cluster details: %v", getClusterErr)
	g.Expect(clusterID).ToNot(gomega.BeEmpty(), "no dataplane cluster found")

	// reload services with manual scaling enabled
	h.Env.Stop()

	// register existing cluster to the data plane cluster config
	db := h.DBFactory().New()
	cluster := api.Cluster{
		ClusterID: clusterID,
	}
	err := db.First(&cluster).Error
	g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get data plane cluster from database: %v", err)

	clusterList = append(clusterList, config.ManualCluster{
		Name:                  "test-cluster",
		ClusterId:             cluster.ClusterID,
		CloudProvider:         cluster.CloudProvider,
		Region:                cluster.Region,
		MultiAZ:               cluster.MultiAZ,
		Schedulable:           true,
		KafkaInstanceLimit:    2,
		Status:                cluster.Status,
		SupportedInstanceType: cluster.SupportedInstanceType,
	})
	enableManualScaling = true

	err = h.Env.CreateServices()
	g.Expect(err).ToNot(gomega.HaveOccurred(), "unable to initialize testing environment: %v", err)

	h.Env.Start()

	// setup pre-requisites for performing requests
	account := h.NewRandAccount()
	ctx := h.NewAuthenticatedContext(account, nil)

	account2 := h.NewAccount("test-user-2", "", "", "test-org-2")

	kafkaRequestPayload := public.KafkaRequestPayload{
		Region:        mocks.MockCluster.Region().ID(),
		CloudProvider: mocks.MockCluster.CloudProvider().ID(),
		Name:          mockKafkaName,
		Plan:          fmt.Sprintf("%s.x1", types.STANDARD.String()),
	}

	testCases := []struct {
		name    string
		setup   func()
		assert  func()
		cleanup func()
	}{
		{
			name: "should successfully create kafka",
			assert: func() {
				kafka, resp, err := common.WaitForKafkaCreateToBeAccepted(ctx, kafkatest.TestServices.DBFactory, client, kafkaRequestPayload)
				if resp != nil {
					resp.Body.Close()
				}
				// kafka successfully registered with database
				g.Expect(err).NotTo(gomega.HaveOccurred(), "Error posting object:  %v", err)
				g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusAccepted))
				g.Expect(kafka.Id).NotTo(gomega.BeEmpty(), "Expected ID assigned on creation")
				g.Expect(kafka.Owner).To(gomega.Equal(account.Username()))
				g.Expect(kafka.Kind).To(gomega.Equal(presenters.KindKafka))
				g.Expect(kafka.Href).To(gomega.Equal(fmt.Sprintf("/api/kafkas_mgmt/v1/kafkas/%s", kafka.Id)))
				g.Expect(kafka.InstanceType).To(gomega.Equal(types.STANDARD.String()))
				g.Expect(kafka.DeprecatedInstanceTypeName).To(gomega.Equal("Standard"))
				g.Expect(kafka.ReauthenticationEnabled).To(gomega.BeTrue())
				g.Expect(kafka.BrowserUrl).To(gomega.Equal(fmt.Sprintf("%s%s/dashboard", kafkatest.TestServices.KafkaConfig.BrowserUrl, kafka.Id)))
				g.Expect(kafka.ExpiresAt).To(gomega.BeNil())
				g.Expect(kafka.AdminApiServerUrl).To(gomega.BeEmpty())

				// wait until the kafka goes into a ready state
				// the timeout here assumes a backing cluster has already been provisioned
				readyKafka, err := common.WaitForKafkaToReachStatus(ctx, kafkatest.TestServices.DBFactory, client, kafka.Id, constants.KafkaRequestStatusReady)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "Error waiting for kafka request to become ready: %v", err)
				g.Expect(readyKafka.BootstrapServerHost).ToNot(gomega.BeEmpty())
				g.Expect(strings.HasSuffix(readyKafka.BootstrapServerHost, ":443")).To(gomega.Equal(true))
				g.Expect(readyKafka.Version).To(gomega.Equal(kasfleetshardsync.GetDefaultReportedKafkaVersion()))
				g.Expect(readyKafka.AdminApiServerUrl).To(gomega.Equal(kasfleetshardsync.AdminServerURI))

				// default kafka max data retention size should be set on creation
				instanceType, err := kafkatest.TestServices.KafkaConfig.SupportedInstanceTypes.Configuration.GetKafkaInstanceTypeByID(readyKafka.InstanceType)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get kafka instance type by id")

				instanceSize, err := instanceType.GetKafkaInstanceSizeByID(kafka.SizeId)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get kafka instance size by id")
				g.Expect(readyKafka.DeprecatedKafkaStorageSize).To(gomega.Equal(instanceSize.MaxDataRetentionSize.String()))

				maxDataRetentionSizeBytes, err := instanceSize.MaxDataRetentionSize.ToInt64()
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to convert max data retention size to bytes")
				g.Expect(readyKafka.MaxDataRetentionSize.Bytes).To(gomega.Equal(maxDataRetentionSizeBytes))

				// check kafka details that's not included in the public kafka request representation
				db := h.DBFactory().New()
				var kafkaRequest dbapi.KafkaRequest
				err = db.Unscoped().Where("id = ?", kafka.Id).First(&kafkaRequest).Error
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to find newly created kafka in the database")

				g.Expect(kafkaRequest.QuotaType).To(gomega.Equal(KafkaConfig(h).Quota.Type))
				g.Expect(kafkaRequest.PlacementId).ToNot(gomega.BeEmpty())
				g.Expect(kafkaRequest.Namespace).To(gomega.Equal(fmt.Sprintf("kafka-%s", strings.ToLower(kafkaRequest.ID))))
				// this is set by the mockKasfFleetshardSync
				g.Expect(kafkaRequest.DesiredStrimziVersion).To(gomega.Equal(kasfleetshardsync.GetDefaultReportedStrimziVersion()))

				common.CheckMetricExposed(h, t, metrics.KafkaCreateRequestDuration)
				common.CheckMetricExposed(h, t, fmt.Sprintf("%s_%s{operation=\"%s\"} 1", metrics.KasFleetManager, metrics.KafkaOperationsSuccessCount, constants2.KafkaOperationCreate.String()))
				common.CheckMetricExposed(h, t, fmt.Sprintf("%s_%s{operation=\"%s\"} 1", metrics.KasFleetManager, metrics.KafkaOperationsTotalCount, constants2.KafkaOperationCreate.String()))
			},
			cleanup: func() {
				// delete test kafka to free up resources on the OSD cluster
				db := h.DBFactory().New().Model(&dbapi.KafkaRequest{})
				var kafkas []*dbapi.KafkaRequest
				err := db.Scan(&kafkas).Error
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get all kafka in the database")

				for _, kafka := range kafkas {
					deleteTestKafka(t, h, ctx, client, kafka.ID)
				}
			},
		},
		{
			name: "should reject kafka request if there is no capacity left",
			setup: func() {
				// Create dummy Kafkas to fill up the cluster capacity
				dummyKafkas := []*dbapi.KafkaRequest{}
				for i := 1; i <= int(clusterList[0].KafkaInstanceLimit); i++ {
					dummyKafkas = append(dummyKafkas, mockkafkas.BuildKafkaRequest(
						mockkafkas.WithPredefinedTestValues(),
						mockkafkas.With(mockkafkas.NAME, fmt.Sprintf("dummy-kafka-%d", i)),
						mockkafkas.With(mockkafkas.CLUSTER_ID, clusterID),
						mockkafkas.With(mockkafkas.CLOUD_PROVIDER, kafkaRequestPayload.CloudProvider),
						mockkafkas.With(mockkafkas.REGION, kafkaRequestPayload.Region),
						mockkafkas.With(mockkafkas.OWNER, account2.Username()),
						mockkafkas.With(mockkafkas.ORGANISATION_ID, account2.Organization().ExternalID()),
						mockkafkas.With(mockkafkas.INSTANCE_TYPE, types.STANDARD.String()),
						mockkafkas.With(mockkafkas.SIZE_ID, "x1"),
					))
				}

				db := h.DBFactory().New()
				err := db.Create(&dummyKafkas).Error
				g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create dummy kafkas")
			},
			assert: func() {
				_, resp, err := common.WaitForKafkaCreateToBeAccepted(ctx, kafkatest.TestServices.DBFactory, client, kafkaRequestPayload)
				if resp != nil {
					resp.Body.Close()
				}
				g.Expect(err).NotTo(gomega.HaveOccurred(), "Error posting object:  %v", err)
				// get a 403 status code as there is no capacity left
				g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusForbidden), "kafka should have been rejected as no capacity is left")
			},
			cleanup: func() {
				// delete dummy kafkas to free up resources on the OSD cluster
				db := h.DBFactory().DB.Model(&dbapi.KafkaRequest{})
				var kafkas []*dbapi.KafkaRequest
				err := db.Scan(&kafkas).Error
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get all kafka in the database")

				ctx := h.NewAuthenticatedContext(account2, nil)

				for _, kafka := range kafkas {
					deleteTestKafka(t, h, ctx, client, kafka.ID)
				}
			},
		},
	}

	for _, tc := range testCases {
		testcase := tc
		t.Run(testcase.name, func(t *testing.T) {
			if testcase.setup != nil {
				testcase.setup()
			}

			testcase.assert()

			if testcase.cleanup != nil {
				testcase.cleanup()
			}
		})
	}
}

// Test Kafka creation with dynamic scaling enabled
func TestKafkaCreate_DynamicScaling(t *testing.T) {
	g := gomega.NewWithT(t)

	ocmServer := mocks.NewMockConfigurableServerBuilder().Build()
	defer ocmServer.Close()

	var enableAutoscale bool
	h, client, teardown := kafkatest.NewKafkaHelperWithHooks(t, ocmServer, func(d *config.DataplaneClusterConfig) {
		if enableAutoscale {
			d.DataPlaneClusterScalingType = config.AutoScaling
			d.DynamicScalingConfig = config.DynamicScalingConfig{
				Configuration: map[string]config.InstanceTypeDynamicScalingConfig{
					"standard": {
						ReservedStreamingUnits: 0,
					},
					"developer": {
						ReservedStreamingUnits: 0,
					},
				},
			}
		}
	})
	defer teardown()

	// set up data plane cluster
	// The mock fleetshard sync only updates the cluster if it's in a non-ready state. Only wait until the cluster has status
	// waiting_for_kas_fleetshard_operator to ensure that the mock fleetshard sync updates the dynamic capacity info correctly.
	clusterID, getClusterErr := common.GetOSDClusterIDAndWaitForStatus(h, t, api.ClusterWaitingForKasFleetShardOperator)
	g.Expect(getClusterErr).ToNot(gomega.HaveOccurred(), "failed to get dataplane cluster details: %v", getClusterErr)
	g.Expect(clusterID).ToNot(gomega.BeEmpty(), "no dataplane cluster found")

	// reload services with auto scaling enabled
	h.Env.Stop()

	enableAutoscale = true
	err := h.Env.CreateServices()
	g.Expect(err).ToNot(gomega.HaveOccurred(), "unable to initialize testing environment: %v", err)

	h.Env.Start()

	// run mock fleetshard sync
	mockKasFleetshardSyncBuilder := kasfleetshardsync.NewMockKasFleetshardSyncBuilder(h, t)
	mockKasFleetshardSync := mockKasFleetshardSyncBuilder.Build()
	mockKasFleetshardSync.Start()
	defer mockKasFleetshardSync.Stop()

	// wait for data plane cluster to reach a ready state
	_, err = common.WaitForClusterStatus(h.DBFactory(), &kafkatest.TestServices.ClusterService, clusterID, api.ClusterReady)
	g.Expect(err).ToNot(gomega.HaveOccurred(), "dataplane cluster failed to reach ready status: %v", err)

	// setup pre-requisites for performing requests
	account := h.NewRandAccount()
	ctx := h.NewAuthenticatedContext(account, nil)

	account2 := h.NewAccount("test-user-2", "", "", "test-org-2")

	kafkaRequestPayload := public.KafkaRequestPayload{
		Region:        mocks.MockCluster.Region().ID(),
		CloudProvider: mocks.MockCluster.CloudProvider().ID(),
		Name:          mockKafkaName,
		Plan:          fmt.Sprintf("%s.x1", types.STANDARD.String()),
	}

	testCases := []struct {
		name    string
		setup   func()
		assert  func()
		cleanup func()
	}{
		{
			name: "should successfully create kafka",
			assert: func() {
				kafka, resp, err := common.WaitForKafkaCreateToBeAccepted(ctx, kafkatest.TestServices.DBFactory, client, kafkaRequestPayload)
				if resp != nil {
					resp.Body.Close()
				}
				// kafka successfully registered with database
				g.Expect(err).NotTo(gomega.HaveOccurred(), "Error posting object:  %v", err)
				g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusAccepted))
				g.Expect(kafka.Id).NotTo(gomega.BeEmpty(), "Expected ID assigned on creation")
				g.Expect(kafka.Owner).To(gomega.Equal(account.Username()))
				g.Expect(kafka.Kind).To(gomega.Equal(presenters.KindKafka))
				g.Expect(kafka.Href).To(gomega.Equal(fmt.Sprintf("/api/kafkas_mgmt/v1/kafkas/%s", kafka.Id)))
				g.Expect(kafka.InstanceType).To(gomega.Equal(types.STANDARD.String()))
				g.Expect(kafka.DeprecatedInstanceTypeName).To(gomega.Equal("Standard"))
				g.Expect(kafka.ReauthenticationEnabled).To(gomega.BeTrue())
				g.Expect(kafka.BrowserUrl).To(gomega.Equal(fmt.Sprintf("%s%s/dashboard", kafkatest.TestServices.KafkaConfig.BrowserUrl, kafka.Id)))
				g.Expect(kafka.ExpiresAt).To(gomega.BeNil())
				g.Expect(kafka.AdminApiServerUrl).To(gomega.BeEmpty())

				// wait until the kafka goes into a ready state
				// the timeout here assumes a backing cluster has already been provisioned
				readyKafka, err := common.WaitForKafkaToReachStatus(ctx, kafkatest.TestServices.DBFactory, client, kafka.Id, constants.KafkaRequestStatusReady)
				g.Expect(err).NotTo(gomega.HaveOccurred(), "Error waiting for kafka request to become ready: %v", err)
				g.Expect(readyKafka.BootstrapServerHost).ToNot(gomega.BeEmpty())
				g.Expect(strings.HasSuffix(readyKafka.BootstrapServerHost, ":443")).To(gomega.Equal(true))
				g.Expect(readyKafka.Version).To(gomega.Equal(kasfleetshardsync.GetDefaultReportedKafkaVersion()))
				g.Expect(readyKafka.AdminApiServerUrl).To(gomega.Equal(kasfleetshardsync.AdminServerURI))

				// default kafka max data retention size should be set on creation
				instanceType, err := kafkatest.TestServices.KafkaConfig.SupportedInstanceTypes.Configuration.GetKafkaInstanceTypeByID(readyKafka.InstanceType)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get kafka instance type by id")

				instanceSize, err := instanceType.GetKafkaInstanceSizeByID(kafka.SizeId)
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get kafka instance size by id")
				g.Expect(readyKafka.DeprecatedKafkaStorageSize).To(gomega.Equal(instanceSize.MaxDataRetentionSize.String()))

				maxDataRetentionSizeBytes, err := instanceSize.MaxDataRetentionSize.ToInt64()
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to convert max data retention size to bytes")
				g.Expect(readyKafka.MaxDataRetentionSize.Bytes).To(gomega.Equal(maxDataRetentionSizeBytes))

				// check kafka details that's not included in the public kafka request representation
				db := h.DBFactory().New()
				var kafkaRequest dbapi.KafkaRequest
				err = db.Unscoped().Where("id = ?", kafka.Id).First(&kafkaRequest).Error
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to find newly created kafka in the database")

				g.Expect(kafkaRequest.QuotaType).To(gomega.Equal(KafkaConfig(h).Quota.Type))
				g.Expect(kafkaRequest.PlacementId).ToNot(gomega.BeEmpty())
				g.Expect(kafkaRequest.Namespace).To(gomega.Equal(fmt.Sprintf("kafka-%s", strings.ToLower(kafkaRequest.ID))))
				// this is set by the mockKasfFleetshardSync
				g.Expect(kafkaRequest.DesiredStrimziVersion).To(gomega.Equal(kasfleetshardsync.GetDefaultReportedStrimziVersion()))

				common.CheckMetricExposed(h, t, metrics.KafkaCreateRequestDuration)
				common.CheckMetricExposed(h, t, fmt.Sprintf("%s_%s{operation=\"%s\"} 1", metrics.KasFleetManager, metrics.KafkaOperationsSuccessCount, constants2.KafkaOperationCreate.String()))
				common.CheckMetricExposed(h, t, fmt.Sprintf("%s_%s{operation=\"%s\"} 1", metrics.KasFleetManager, metrics.KafkaOperationsTotalCount, constants2.KafkaOperationCreate.String()))
			},
			cleanup: func() {
				// delete test kafka to free up resources on the OSD cluster
				db := h.DBFactory().New().Model(&dbapi.KafkaRequest{})
				var kafkas []*dbapi.KafkaRequest
				err := db.Scan(&kafkas).Error
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get all kafka in the database")

				for _, kafka := range kafkas {
					deleteTestKafka(t, h, ctx, client, kafka.ID)
				}
			},
		},
		{
			name: "should reject kafka request if there is no capacity left",
			setup: func() {
				// Create dummy Kafkas to fill up the cluster capacity
				dummyKafkas := []*dbapi.KafkaRequest{}
				for i := 1; i <= int(kasfleetshardsync.StandardCapacityInfo.MaxUnits); i++ {
					dummyKafkas = append(dummyKafkas, mockkafkas.BuildKafkaRequest(
						mockkafkas.WithPredefinedTestValues(),
						mockkafkas.With(mockkafkas.NAME, fmt.Sprintf("dummy-kafka-%d", i)),
						mockkafkas.With(mockkafkas.CLUSTER_ID, clusterID),
						mockkafkas.With(mockkafkas.CLOUD_PROVIDER, kafkaRequestPayload.CloudProvider),
						mockkafkas.With(mockkafkas.REGION, kafkaRequestPayload.Region),
						mockkafkas.With(mockkafkas.OWNER, account2.Username()),
						mockkafkas.With(mockkafkas.ORGANISATION_ID, account2.Organization().ExternalID()),
						mockkafkas.With(mockkafkas.INSTANCE_TYPE, types.STANDARD.String()),
						mockkafkas.With(mockkafkas.SIZE_ID, "x1"),
					))
				}

				db := h.DBFactory().New()
				err := db.Create(&dummyKafkas).Error
				g.Expect(err).NotTo(gomega.HaveOccurred(), "failed to create dummy kafkas")
			},
			assert: func() {
				_, resp, err := common.WaitForKafkaCreateToBeAccepted(ctx, kafkatest.TestServices.DBFactory, client, kafkaRequestPayload)
				if resp != nil {
					resp.Body.Close()
				}
				g.Expect(err).NotTo(gomega.HaveOccurred(), "Error posting object:  %v", err)
				// get a 403 status code as there is no capacity left
				g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusForbidden), "kafka should have been rejected as no capacity is left")
			},
			cleanup: func() {
				// delete dummy kafkas to free up resources on the OSD cluster
				db := h.DBFactory().DB.Model(&dbapi.KafkaRequest{})
				var kafkas []*dbapi.KafkaRequest
				err := db.Scan(&kafkas).Error
				g.Expect(err).ToNot(gomega.HaveOccurred(), "failed to get all kafka in the database")

				ctx := h.NewAuthenticatedContext(account2, nil)

				for _, kafka := range kafkas {
					deleteTestKafka(t, h, ctx, client, kafka.ID)
				}
			},
		},
	}

	for _, tc := range testCases {
		testcase := tc
		t.Run(testcase.name, func(t *testing.T) {
			if testcase.setup != nil {
				testcase.setup()
			}

			testcase.assert()

			if testcase.cleanup != nil {
				testcase.cleanup()
			}
		})
	}
}

func TestKafkaCreate_ValidatePlanParam(t *testing.T) {
	g := gomega.NewWithT(t)

	ocmServer := mocks.NewMockConfigurableServerBuilder().Build()
	defer ocmServer.Close()

	h, client, teardown := kafkatest.NewKafkaHelperWithHooks(t, ocmServer, nil)
	defer teardown()

	mockKasFleetshardSyncBuilder := kasfleetshardsync.NewMockKasFleetshardSyncBuilder(h, t)
	mockKasfFleetshardSync := mockKasFleetshardSyncBuilder.Build()
	mockKasfFleetshardSync.Start()
	defer mockKasfFleetshardSync.Stop()

	clusterID, getClusterErr := common.GetRunningOsdClusterID(h, t)
	if getClusterErr != nil {
		t.Fatalf("Failed to retrieve cluster details: %v", getClusterErr)
	}
	if clusterID == "" {
		panic("No cluster found")
	}

	account := h.NewRandAccount()
	ctx := h.NewAuthenticatedContext(account, nil)

	k := public.KafkaRequestPayload{
		Region:        mocks.MockCluster.Region().ID(),
		CloudProvider: mocks.MockCluster.CloudProvider().ID(),
		Name:          mockKafkaName,
		Plan:          fmt.Sprintf("%s.x1", types.STANDARD.String()),
	}

	kafka, resp, err := client.DefaultApi.CreateKafka(ctx, true, k)
	if resp != nil {
		resp.Body.Close()
	}
	// successful creation of kafka with a valid "standard" plan format
	g.Expect(err).NotTo(gomega.HaveOccurred(), "Error posting object:  %v", err)
	g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusAccepted))
	g.Expect(kafka.Id).NotTo(gomega.BeEmpty(), "g.Expected ID assigned on creation")
	g.Expect(kafka.InstanceType).To(gomega.Equal(types.STANDARD.String()))
	g.Expect(kafka.MultiAz).To(gomega.BeTrue())
	g.Expect(kafka.ExpiresAt).To(gomega.BeNil())

	// successful creation of kafka with valid "developer plan format
	k2 := public.KafkaRequestPayload{
		Region:        mocks.MockCluster.Region().ID(),
		CloudProvider: mocks.MockCluster.CloudProvider().ID(),
		Name:          "test-kafka-2",
		Plan:          fmt.Sprintf("%s.x1", types.DEVELOPER.String()),
	}
	accountWithoutStandardInstances := h.NewAccountWithNameAndOrg("test-nameacc-2", "123456")
	ctx2 := h.NewAuthenticatedContext(accountWithoutStandardInstances, nil)
	kafka, resp, err = client.DefaultApi.CreateKafka(ctx2, true, k2)
	if resp != nil {
		resp.Body.Close()
	}
	g.Expect(err).NotTo(gomega.HaveOccurred(), "Error posting object:  %v", err)
	g.Expect(resp.StatusCode).To(gomega.Equal(http.StatusAccepted))
	g.Expect(kafka.Id).NotTo(gomega.BeEmpty(), "g.Expected ID assigned on creation")
	g.Expect(kafka.InstanceType).To(gomega.Equal(types.DEVELOPER.String()))
	g.Expect(kafka.MultiAz).To(gomega.BeFalse())
	// Verify that developer instances should have an expiration time set
	g.Expect(kafka.ExpiresAt).NotTo(gomega.BeNil())
	instanceTypeConfig, err := kafkatest.TestServices.KafkaConfig.SupportedInstanceTypes.Configuration.GetKafkaInstanceTypeByID(kafka.InstanceType)
	g.Expect(err).ToNot(gomega.HaveOccurred())
	instanceTypeSizeConfig, err := instanceTypeConfig.GetKafkaInstanceSizeByID("x1")
	g.Expect(err).ToNot(gomega.HaveOccurred())
	g.Expect(*kafka.ExpiresAt).To(gomega.Equal(kafka.CreatedAt.Add(time.Duration(*instanceTypeSizeConfig.LifespanSeconds) * time.Second)))

	// unsuccessful creation of kafka with invalid instance type provided in the "plan" parameter
	k.Plan = "invalid.x1"
	kafka, resp, err = client.DefaultApi.CreateKafka(ctx, true, k)
	if resp != nil {
		resp.Body.Close()
	}
	g.Expect(err).To(gomega.HaveOccurred(), "Error should have occurred when attempting to create kafka with invalid instance type provided in the plan")

	// unsuccessful creation of kafka with invalid size_id provided in the "plan" parameter
	k.Plan = fmt.Sprintf("%s.x9999", types.STANDARD.String())
	kafka, resp, err = client.DefaultApi.CreateKafka(ctx, true, k)
	if resp != nil {
		resp.Body.Close()
	}
	g.Expect(err).To(gomega.HaveOccurred(), "Error should have occurred when attempting to create kafka unsupported size_id")
}
