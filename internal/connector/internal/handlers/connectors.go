package handlers

import (
	"encoding/json"
	"fmt"
	presenters2 "github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/connector/internal/presenters"
	services2 "github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/connector/internal/services"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/db"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/handlers"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/logger"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/shared/secrets"
	"github.com/spyzhov/ajson"
	"io/ioutil"
	"net/http"
	"reflect"

	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/api"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/api/connector/openapi"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/auth"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/errors"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/services"
	jsonpatch "github.com/evanphx/json-patch"
	"github.com/golang/glog"
	"github.com/gorilla/mux"
)

var (
	maxKafkaNameLength   = 32
	maxConnectorIdLength = 32
)

type ConnectorsHandler struct {
	connectorsService     services2.ConnectorsService
	connectorTypesService services2.ConnectorTypesService
	kafkaService          services.KafkaService
	vaultService          services.VaultService
}

func NewConnectorsHandler(kafkaService services.KafkaService, connectorsService services2.ConnectorsService, connectorTypesService services2.ConnectorTypesService, vaultService services.VaultService) *ConnectorsHandler {
	return &ConnectorsHandler{
		kafkaService:          kafkaService,
		connectorsService:     connectorsService,
		connectorTypesService: connectorTypesService,
		vaultService:          vaultService,
	}
}

func (h ConnectorsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var resource openapi.Connector
	tid := mux.Vars(r)["tid"]
	cfg := &handlers.HandlerConfig{

		MarshalInto: &resource,
		Validate: []handlers.Validate{
			handlers.ValidateAsyncEnabled(r, "creating connector"),
			handlers.Validation("channel", &resource.Channel, handlers.WithDefault("stable"), handlers.MaxLen(40)),
			handlers.Validation("metadata.name", &resource.Metadata.Name,
				handlers.WithDefault("New Connector"), handlers.MinLen(1), handlers.MaxLen(100)),
			handlers.Validation("metadata.kafka_id", &resource.Metadata.KafkaId, handlers.MinLen(1), handlers.MaxLen(maxKafkaNameLength)),
			handlers.Validation("kafka.bootstrap_server", &resource.Kafka.BootstrapServer, handlers.MinLen(1)),
			handlers.Validation("kafka.client_id", &resource.Kafka.ClientId, handlers.MinLen(1)),
			handlers.Validation("kafka.client_secret", &resource.Kafka.ClientSecret, handlers.MinLen(1)),
			handlers.Validation("connector_type_id", &resource.ConnectorTypeId, handlers.MinLen(1), handlers.MaxLen(maxConnectorTypeIdLength)),
			handlers.Validation("desired_state", &resource.DesiredState, handlers.WithDefault("ready"), handlers.IsOneOf("ready", "stopped")),
			validateConnectorSpec(h.connectorTypesService, &resource, tid),
		},

		Action: func() (interface{}, *errors.ServiceError) {

			// Get the Kafka to assert the user can access that kafka instance.
			_, err := h.kafkaService.Get(r.Context(), resource.Metadata.KafkaId)
			if err != nil {
				return nil, err
			}

			convResource, err := presenters2.ConvertConnector(resource)
			if err != nil {
				return nil, err
			}

			claims, goerr := auth.GetClaimsFromContext(r.Context())
			if goerr != nil {
				return nil, errors.Unauthenticated("user not authenticated")
			}

			convResource.ID = api.NewID()
			convResource.Owner = auth.GetUsernameFromClaims(claims)
			convResource.OrganisationId = auth.GetOrgIdFromClaims(claims)

			ct, err := h.connectorTypesService.Get(resource.ConnectorTypeId)
			if err != nil {
				return nil, errors.BadRequest("invalid connector type id: %s", resource.ConnectorTypeId)
			}

			err = moveSecretsToVault(convResource, ct, h.vaultService, true)
			if err != nil {
				return nil, err
			}

			if svcErr := h.connectorsService.Create(r.Context(), convResource); svcErr != nil {
				return nil, svcErr
			}

			if err := stripSecretReferences(convResource, ct); err != nil {
				return nil, err
			}

			return presenters2.PresentConnector(convResource)
		},
	}

	// return 202 status accepted
	handlers.Handle(w, r, cfg, http.StatusAccepted)
}

func (h ConnectorsHandler) Patch(w http.ResponseWriter, r *http.Request) {

	connectorId := mux.Vars(r)["connector_id"]
	connectorTypeId := mux.Vars(r)["connector_type_id"]
	contentType := r.Header.Get("Content-Type")

	cfg := &handlers.HandlerConfig{
		Validate: []handlers.Validate{
			handlers.Validation("connector_id", &connectorId, handlers.MinLen(1), handlers.MaxLen(maxConnectorIdLength)),
			handlers.Validation("connector_type_id", &connectorTypeId, handlers.MaxLen(maxConnectorTypeIdLength)),
			handlers.Validation("Content-Type header", &contentType, handlers.IsOneOf("application/json-patch+json", "application/merge-patch+json")),
		},
		Action: func() (interface{}, *errors.ServiceError) {

			dbresource, serr := h.connectorsService.Get(r.Context(), connectorId, connectorTypeId)
			if serr != nil {
				return nil, serr
			}

			resource, serr := presenters2.PresentConnector(dbresource)
			if serr != nil {
				return nil, serr
			}

			ct, serr := h.connectorTypesService.Get(dbresource.ConnectorTypeId)
			if serr != nil {
				return nil, errors.BadRequest("invalid connector type id: %s", resource.ConnectorTypeId)
			}

			originalSecrets, err := getSecretRefs(dbresource, ct)
			if err != nil {
				return nil, errors.GeneralError("could not get existing secrets: %v", err)
			}

			// Apply the patch..
			patchBytes, err := ioutil.ReadAll(r.Body)
			if err != nil {
				return nil, errors.BadRequest("failed to get patch bytes")
			}

			// Don't allow updating connector secrets with values like {"ref": "something"}
			serr = validateConnectorPatch(patchBytes, ct)
			if serr != nil {
				return nil, serr
			}

			patch := openapi.Connector{}
			serr = PatchResource(resource, contentType, patchBytes, &patch)
			if serr != nil {
				return nil, serr
			}

			// But we don't want to allow the user to update ALL fields.. so copy
			// over the fields that they are allowed to modify..
			resource.Metadata.Name = patch.Metadata.Name
			resource.Metadata.KafkaId = patch.Metadata.KafkaId
			resource.ConnectorSpec = patch.ConnectorSpec
			resource.DesiredState = patch.DesiredState
			resource.Metadata.ResourceVersion = patch.Metadata.ResourceVersion
			resource.Kafka = patch.Kafka

			// If we didn't change anything, then just skip the update...
			originalResource, _ := presenters2.PresentConnector(dbresource)
			if reflect.DeepEqual(originalResource, resource) {
				return originalResource, nil
			}

			// revalidate
			validates := []handlers.Validate{
				handlers.Validation("name", &resource.Metadata.Name, handlers.MinLen(1), handlers.MaxLen(100)),
				handlers.Validation("connector_type_id", &resource.ConnectorTypeId, handlers.MinLen(1), handlers.MaxLen(maxKafkaNameLength)),
				handlers.Validation("kafka_id", &resource.Metadata.KafkaId, handlers.MinLen(1), handlers.MaxLen(maxKafkaNameLength)),
				handlers.Validation("Kafka client_id", &resource.Kafka.ClientId, handlers.MinLen(1)),
				validateConnectorSpec(h.connectorTypesService, &resource, connectorTypeId),
			}

			for _, v := range validates {
				err := v()
				if err != nil {
					return nil, err
				}
			}

			// did the kafka instance change? verify we can use it...
			if resource.Metadata.KafkaId != originalResource.Metadata.KafkaId {
				_, err := h.kafkaService.Get(r.Context(), resource.Metadata.KafkaId)
				if err != nil {
					return nil, err
				}
			}

			p, svcErr := presenters2.ConvertConnector(resource)
			if svcErr != nil {
				return nil, svcErr
			}

			svcErr = moveSecretsToVault(p, ct, h.vaultService, false)
			if svcErr != nil {
				return nil, svcErr
			}

			serr = h.connectorsService.Update(r.Context(), p)
			if serr != nil {
				return nil, serr
			}

			if originalResource.Status != api.ConnectorStatusPhaseAssigning {
				dbresource.Status.Phase = api.ConnectorStatusPhaseUpdating
				p.Status.Phase = api.ConnectorStatusPhaseUpdating
				serr = h.connectorsService.SaveStatus(r.Context(), dbresource.Status)
				if serr != nil {
					return nil, serr
				}
			}

			newSecrets, err := getSecretRefs(p, ct)
			if err != nil {
				return nil, errors.GeneralError("could not get existing secrets: %v", err)
			}

			staleSecrets := StringListSubtract(originalSecrets, newSecrets...)
			if len(staleSecrets) > 0 {
				_ = db.AddPostCommitAction(r.Context(), func() {
					for _, s := range staleSecrets {
						err = h.vaultService.DeleteSecretString(s)
						if err != nil {
							logger.Logger.Errorf("failed to delete vault secret key '%s': %v", s, err)
						}
					}
				})
			}

			if err := stripSecretReferences(p, ct); err != nil {
				return nil, err
			}

			return presenters2.PresentConnector(p)
		},
	}

	// return 202 status accepted
	handlers.Handle(w, r, cfg, http.StatusAccepted)
}

func validateConnectorPatch(bytes []byte, ct *api.ConnectorType) *errors.ServiceError {
	type Connector struct {
		ConnectorSpec api.JSON `json:"connector_spec,omitempty"`
	}
	c := Connector{}
	err := json.Unmarshal(bytes, &c)
	if err != nil {
		return errors.BadRequest("invalid patch: %v", err)
	}

	if len(c.ConnectorSpec) > 0 {
		_, err := secrets.ModifySecrets(ct.JsonSchema, c.ConnectorSpec, func(node *ajson.Node) error {
			if node.Type() == ajson.Object {
				if len(node.Keys()) > 0 {
					return fmt.Errorf("attempting to change opaque connector secret")
				}
			}
			return nil
		})
		if err != nil {
			return errors.BadRequest("invalid patch: %v", err)
		}
	}
	return nil
}

func StringListSubtract(l []string, items ...string) (result []string) {
	m := make(map[string]struct{}, len(l))
	for _, x := range l {
		m[x] = struct{}{}
	}
	for _, item := range items {
		delete(m, item)
	}
	for item := range m {
		result = append(result, item)
	}
	return result
}

func PatchResource(resource interface{}, patchType string, patchBytes []byte, patched interface{}) *errors.ServiceError {
	if reflect.ValueOf(resource).Kind() == reflect.Ptr {
		resource = reflect.ValueOf(resource).Elem() // deref pointer...
	}
	resourceJson, err := json.Marshal(resource)
	if err != nil {
		return errors.GeneralError("failed to encode to json")
	}

	// apply the patch...
	switch patchType {
	case "application/json-patch+json":
		var patchObj jsonpatch.Patch
		patchObj, err := jsonpatch.DecodePatch(patchBytes)
		if err != nil {
			return errors.BadRequest("invalid json patch: %v", err)
		}
		resourceJson, err = patchObj.Apply(resourceJson)
		if err != nil {
			return errors.GeneralError("failed to apply patch patch: %v", err)
		}
	case "application/merge-patch+json":
		resourceJson, err = jsonpatch.MergePatch(resourceJson, patchBytes)
		if err != nil {
			return errors.BadRequest("invalid json patch: %v", err)
		}
	default:
		return errors.BadRequest("bad Content-Type, must be one of: application/json-patch+json or application/merge-patch+json")
	}

	err = json.Unmarshal(resourceJson, patched)
	if err != nil {
		return errors.GeneralError("failed to decode patched resource: %v", err)
	}
	return nil
}

func (h ConnectorsHandler) Get(w http.ResponseWriter, r *http.Request) {
	connectorId := mux.Vars(r)["connector_id"]
	connectorTypeId := mux.Vars(r)["connector_type_id"]
	cfg := &handlers.HandlerConfig{
		Validate: []handlers.Validate{
			handlers.Validation("connector_id", &connectorId, handlers.MinLen(1), handlers.MaxLen(maxConnectorIdLength)),
			handlers.Validation("connector_type_id", &connectorTypeId, handlers.MaxLen(maxConnectorTypeIdLength)),
		},
		Action: func() (i interface{}, serviceError *errors.ServiceError) {
			resource, err := h.connectorsService.Get(r.Context(), connectorId, connectorTypeId)
			if err != nil {
				return nil, err
			}

			ct, serr := h.connectorTypesService.Get(resource.ConnectorTypeId)
			if serr != nil {
				return nil, errors.BadRequest("invalid connector type id: %s", resource.ConnectorTypeId)
			}

			if err := stripSecretReferences(resource, ct); err != nil {
				return nil, err
			}

			return presenters2.PresentConnector(resource)
		},
	}
	handlers.HandleGet(w, r, cfg)
}

// Delete is the handler for deleting a kafka request
func (h ConnectorsHandler) Delete(w http.ResponseWriter, r *http.Request) {
	connectorId := mux.Vars(r)["connector_id"]
	cfg := &handlers.HandlerConfig{
		Validate: []handlers.Validate{
			handlers.Validation("connector_id", &connectorId, handlers.MinLen(1), handlers.MaxLen(maxConnectorIdLength)),
		},
		Action: func() (i interface{}, serviceError *errors.ServiceError) {

			c, err := h.connectorsService.Get(r.Context(), connectorId, "")
			if err != nil {
				return nil, err
			}
			if c.DesiredState != "deleted" {
				c.DesiredState = "deleted"
				err := h.connectorsService.Update(r.Context(), c)
				if err != nil {
					return nil, err
				}
			}

			return nil, nil
		},
	}
	handlers.HandleDelete(w, r, cfg, http.StatusNoContent)
}

func (h ConnectorsHandler) List(w http.ResponseWriter, r *http.Request) {
	kafkaId := r.URL.Query().Get("kafka_id")
	connectorTypeId := mux.Vars(r)["connector_type_id"]
	cfg := &handlers.HandlerConfig{
		Validate: []handlers.Validate{
			handlers.Validation("kafka_id", &kafkaId, handlers.MaxLen(maxKafkaNameLength)),
			handlers.Validation("connector_type_id", &connectorTypeId, handlers.MaxLen(maxConnectorTypeIdLength)),
		},
		Action: func() (interface{}, *errors.ServiceError) {
			ctx := r.Context()
			listArgs := services.NewListArguments(r.URL.Query())
			resources, paging, err := h.connectorsService.List(ctx, kafkaId, listArgs, connectorTypeId)
			if err != nil {
				return nil, err
			}

			resourceList := openapi.ConnectorList{
				Kind:  "ConnectorList",
				Page:  int32(paging.Page),
				Size:  int32(paging.Size),
				Total: int32(paging.Total),
			}

			for _, resource := range resources {

				ct, serr := h.connectorTypesService.Get(resource.ConnectorTypeId)
				if serr != nil {
					return nil, errors.BadRequest("invalid connector type id: %s", resource.ConnectorTypeId)
				}

				if err := stripSecretReferences(resource, ct); err != nil {
					return nil, err
				}
				converted, err := presenters2.PresentConnector(resource)
				if err != nil {
					glog.Errorf("connector id='%s' presentation failed: %v", resource.ID, err)
					return nil, errors.GeneralError("internal error")
				}
				resourceList.Items = append(resourceList.Items, converted)

			}

			return resourceList, nil
		},
	}

	handlers.HandleList(w, r, cfg)
}