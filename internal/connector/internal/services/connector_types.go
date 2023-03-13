package services

//
// Unlike most other service types that are backed by a DB, this service
// reports back information about the supported connector types and they
// become available when as supporting services are deployed to the control
// pane cluster.

import (
	"database/sql"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/shared/utils/arrays"
	"gorm.io/gorm/clause"
	"k8s.io/apimachinery/pkg/util/sets"
	"regexp"
	"strings"

	"gorm.io/gorm"

	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/connector/internal/api/dbapi"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/connector/internal/config"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/internal/connector/internal/presenters"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/api"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/db"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/errors"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/services"
	coreService "github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/services"
	"github.com/bf2fc6cc711aee1a0c2a/kas-fleet-manager/pkg/services/queryparser"
	"github.com/golang/glog"
)

type ConnectorTypesService interface {
	Get(id string) (*dbapi.ConnectorType, *errors.ServiceError)
	List(listArgs *services.ListArguments) (dbapi.ConnectorTypeList, *api.PagingMeta, *errors.ServiceError)
	ListLabels(listArgs *services.ListArguments) (dbapi.ConnectorTypeLabelCountList, *errors.ServiceError)
	ForEachConnectorCatalogEntry(f func(id string, channel string, ccc *config.ConnectorChannelConfig) *errors.ServiceError) *errors.ServiceError

	PutConnectorShardMetadata(ctc *dbapi.ConnectorShardMetadata) (int64, *errors.ServiceError)
	GetConnectorShardMetadata(typeId, channel string, revision int64) (*dbapi.ConnectorShardMetadata, *errors.ServiceError)
	GetLatestConnectorShardMetadata(typeId, channel string) (*dbapi.ConnectorShardMetadata, *errors.ServiceError)
	CatalogEntriesReconciled() (bool, *errors.ServiceError)
	DeleteOrDeprecateRemovedTypes() *errors.ServiceError
	ListCatalogEntries(*coreService.ListArguments) ([]dbapi.ConnectorCatalogEntry, *api.PagingMeta, *errors.ServiceError)
	GetCatalogEntry(tyd string) (*dbapi.ConnectorCatalogEntry, *errors.ServiceError)
}

var _ ConnectorTypesService = &connectorTypesService{}

type connectorTypesService struct {
	connectorsConfig  *config.ConnectorsConfig
	connectionFactory *db.ConnectionFactory
}

func NewConnectorTypesService(connectorsConfig *config.ConnectorsConfig, connectionFactory *db.ConnectionFactory) *connectorTypesService {
	return &connectorTypesService{
		connectorsConfig:  connectorsConfig,
		connectionFactory: connectionFactory,
	}
}

// Create updates/inserts a connector type in the database
func (cts *connectorTypesService) Create(resource *dbapi.ConnectorType) *errors.ServiceError {
	tid := resource.ID
	if tid == "" {
		return errors.Validation("connector type id is undefined")
	}

	// perform all type related DB updates in a single TX
	if err := cts.connectionFactory.New().Transaction(func(dbConn *gorm.DB) error {

		var oldResource dbapi.ConnectorType
		if err := dbConn.Select("id").Preload("Annotations").
			Where("id = ?", tid).First(&oldResource).Error; err != nil {

			if services.IsRecordNotFoundError(err) {
				// We need to create the resource....
				if err := dbConn.Create(resource).Error; err != nil {
					return errors.GeneralError("failed to create connector type %q: %v", tid, err)
				}
			} else {
				return errors.NewWithCause(errors.ErrorGeneral, err, "unable to find connector type")
			}

		} else {
			// remove old associations first
			if err := dbConn.Where("connector_type_id = ?", tid).Delete(&dbapi.ConnectorTypeAnnotation{}).Error; err != nil {
				return errors.GeneralError("failed to remove connector type annotations %q: %v", tid, err)
			}
			if err := dbConn.Where("connector_type_id = ?", tid).Delete(&dbapi.ConnectorTypeLabel{}).Error; err != nil {
				return errors.GeneralError("failed to remove connector type labels %q: %v", tid, err)
			}
			if err := dbConn.Exec("DELETE FROM connector_type_channels WHERE connector_type_id = ?", tid).Error; err != nil {
				return errors.GeneralError("failed to remove connector type channels %q: %v", tid, err)
			}
			if err := dbConn.Where("connector_type_id = ?", tid).Delete(&dbapi.ConnectorTypeCapability{}).Error; err != nil {
				return errors.GeneralError("failed to remove connector type capabilities %q: %v", tid, err)
			}

			// update the existing connector type
			if err := dbConn.Session(&gorm.Session{FullSaveAssociations: true}).Updates(resource).Error; err != nil {
				return errors.GeneralError("failed to update connector type %q: %v", tid, err)
			}

			// update connector annotations
			if err := updateConnectorAnnotations(dbConn, oldResource); err != nil {
				return err
			}
		}

		// read it back.... to get the updated version...
		if err := dbConn.Where("id = ?", tid).
			Preload(clause.Associations).
			First(&resource).Error; err != nil {
			return services.HandleGetError("Connector type", "id", tid, err)
		}

		return nil
	}); err != nil {
		switch se := err.(type) {
		case *errors.ServiceError:
			return se
		default:
			return errors.GeneralError("failed to create/update connector type %q: %v", tid, se.Error())
		}
	}

	return nil
}

func updateConnectorAnnotations(dbConn *gorm.DB, oldResource dbapi.ConnectorType) error {
	// delete old type annotations copied to connectors
	oldKeys := arrays.Map(oldResource.Annotations, func(ann dbapi.ConnectorTypeAnnotation) string {
		return ann.Key
	})
	tid := oldResource.ID
	if err := dbConn.Exec("DELETE FROM connector_annotations ca "+
		"USING connectors c WHERE c.deleted_at IS NULL AND ca.connector_id = c.id AND "+
		"c.connector_type_id = ? AND ca.key IN ?", tid, oldKeys).Error; err != nil {
		return errors.GeneralError("failed to delete old connector annotations related to type %q: %v", tid, err)
	}
	// copy new type annotations to connectors
	if err := dbConn.Exec("INSERT INTO connector_annotations (connector_id, key, value) "+
		"SELECT c.id, ct.key, ct.value FROM connector_type_annotations ct "+
		"JOIN connectors c ON c.deleted_at IS NULL AND c.connector_type_id = ct.connector_type_id AND "+
		"ct.connector_type_id = ?", tid).Error; err != nil {
		return errors.GeneralError("failed to create new connector annotations related to type %q: %v", tid, err)
	}
	return nil
}

func (cts *connectorTypesService) Get(id string) (*dbapi.ConnectorType, *errors.ServiceError) {
	if id == "" {
		return nil, errors.Validation("typeId is empty")
	}

	var resource dbapi.ConnectorType
	dbConn := cts.connectionFactory.New()

	if err := dbConn.Unscoped().
		Preload(clause.Associations).
		Where("connector_types.id = ?", id).
		First(&resource).Error; err != nil {
		return nil, services.HandleGetError(`Connector type`, `id`, id, err)
	}
	if resource.DeletedAt.Valid {
		return nil, services.HandleGoneError("Connector type", "id", id)
	}
	return &resource, nil
}

func GetValidConnectorTypeColumns() []string {
	return []string{"id", "created_at", "updated_at", "version", "name", "description", "label", "channel", "featured_rank", "pricing_tier", "deprecated"}
}

var skipOrderByColumnsRegExp = regexp.MustCompile("^(channel)|(label)|(pricing_tier)")

var labelSetSearchClause = regexp.MustCompile("label [Ii]?[Ll][Ii][Kk][Ee] ")

// List returns all connector types
func (cts *connectorTypesService) List(listArgs *services.ListArguments) (dbapi.ConnectorTypeList, *api.PagingMeta, *errors.ServiceError) {
	if err := listArgs.Validate(GetValidConnectorTypeColumns()); err != nil {
		return nil, nil, errors.NewWithCause(errors.ErrorMalformedRequest, err, "Unable to list connector type requests: %s", err.Error())
	}

	//var resourceList dbapi.ConnectorTypeList
	var resourceList dbapi.ConnectorTypeList
	dbConn := cts.connectionFactory.New()
	pagingMeta := &api.PagingMeta{
		Page: listArgs.Page,
		Size: listArgs.Size,
	}

	dbConn = dbConn.Joins("LEFT JOIN connector_type_channels channels on channels.connector_type_id = connector_types.id")
	dbConn = dbConn.Where("channels.connector_channel_channel IN ?", cts.connectorsConfig.ConnectorsSupportedChannels)
	dbConn.Group("connector_types.id")

	// Apply search query
	if len(listArgs.Search) > 0 {
		queryParser := queryparser.NewQueryParser(GetValidConnectorTypeColumns()...)
		searchDbQuery, err := queryParser.Parse(listArgs.Search)
		if err != nil {
			return resourceList, pagingMeta, errors.NewWithCause(errors.ErrorFailedToParseSearch, err, "Unable to list connector type requests: %s", err.Error())
		}
		if strings.Contains(searchDbQuery.Query, "channel") {
			searchDbQuery.Query = strings.ReplaceAll(searchDbQuery.Query, "channel", "channels.connector_channel_channel")
		}
		if strings.Contains(searchDbQuery.Query, "label") {
			if labelSetSearchClause.MatchString(searchDbQuery.Query) {
				dbConn = dbConn.Joins("LEFT JOIN (select connector_type_id, string_agg(label, ',' order by label) as label from connector_type_labels group by connector_type_id) as labels on labels.connector_type_id = connector_types.id")
			} else {
				dbConn = dbConn.Joins("LEFT JOIN connector_type_labels labels on labels.connector_type_id = connector_types.id")
			}
			searchDbQuery.Query = strings.ReplaceAll(searchDbQuery.Query, "label", "labels.label")
		}
		if strings.Contains(searchDbQuery.Query, "pricing_tier") {
			dbConn = dbConn.Joins("LEFT JOIN connector_type_annotations annotations on annotations.connector_type_id = connector_types.id")
			searchDbQuery.Query = strings.ReplaceAll(searchDbQuery.Query, "pricing_tier", "annotations.key = 'cos.bf2.org/pricing-tier' and annotations.value")
		}
		dbConn = dbConn.Where(searchDbQuery.Query, searchDbQuery.Values...)
	}

	if len(listArgs.OrderBy) == 0 {
		// default orderBy name
		dbConn = dbConn.Order("name ASC")
	}

	// Set the order by arguments if any
	for _, orderByArg := range listArgs.OrderBy {
		// ignore unsupported columns in orderBy
		if skipOrderByColumnsRegExp.MatchString(orderByArg) {
			continue
		}
		dbConn = dbConn.Order(orderByArg)
	}

	// set total, limit and paging (based on https://gitlab.cee.redhat.com/service/api-guidelines#user-content-paging)
	total := int64(pagingMeta.Total)
	dbConn.Model(&resourceList).Count(&total)
	pagingMeta.Total = int(dbConn.RowsAffected)
	if pagingMeta.Size > pagingMeta.Total {
		pagingMeta.Size = pagingMeta.Total
	}
	dbConn = dbConn.Offset((pagingMeta.Page - 1) * pagingMeta.Size).Limit(pagingMeta.Size)

	// execute query
	result := dbConn.
		Preload(clause.Associations).
		Find(&resourceList)
	if result.Error != nil {
		return nil, nil, errors.ToServiceError(result.Error)
	}

	return resourceList, pagingMeta, nil
}

// ListLabels returns a list of all label names and count of labels for matching search query
func (cts *connectorTypesService) ListLabels(listArgs *services.ListArguments) (dbapi.ConnectorTypeLabelCountList, *errors.ServiceError) {
	if err := listArgs.Validate(GetValidConnectorTypeColumns()); err != nil {
		return nil, errors.NewWithCause(errors.ErrorMalformedRequest, err, "unable to list connector type labels requests: %s", err.Error())
	}

	var resourceList dbapi.ConnectorTypeLabelCountList
	// two connections for labels and featured connectors count queries
	dbConn := cts.connectionFactory.New()

	dbConn = dbConn.Joins("LEFT JOIN connector_type_channels channels on channels.connector_type_id = connector_types.id")
	dbConn = dbConn.Where("channels.connector_channel_channel IN ?", cts.connectorsConfig.ConnectorsSupportedChannels)
	dbConn.Group("connector_types.id")

	// Apply search query
	if len(listArgs.Search) > 0 {
		queryParser := queryparser.NewQueryParser(GetValidConnectorTypeColumns()...)
		searchDbQuery, err := queryParser.Parse(listArgs.Search)
		if err != nil {
			return resourceList, errors.NewWithCause(errors.ErrorFailedToParseSearch, err, "unable to list connector type labels requests: %s", err.Error())
		}
		if strings.Contains(searchDbQuery.Query, "channel") {
			searchDbQuery.Query = strings.ReplaceAll(searchDbQuery.Query, "channel", "channels.connector_channel_channel")
		}
		if strings.Contains(searchDbQuery.Query, "label") {
			if labelSetSearchClause.MatchString(searchDbQuery.Query) {
				dbConn = dbConn.Joins("LEFT JOIN (select connector_type_id, string_agg(label, ',' order by label) as label from connector_type_labels group by connector_type_id) as labels on labels.connector_type_id = connector_types.id")
			} else {
				dbConn = dbConn.Joins("LEFT JOIN connector_type_labels labels on labels.connector_type_id = connector_types.id")
			}
			searchDbQuery.Query = strings.ReplaceAll(searchDbQuery.Query, "label", "labels.label")
		}
		if strings.Contains(searchDbQuery.Query, "pricing_tier") {
			dbConn = dbConn.Joins("LEFT JOIN connector_type_annotations annotations on annotations.connector_type_id = connector_types.id")
			searchDbQuery.Query = strings.ReplaceAll(searchDbQuery.Query, "pricing_tier", "annotations.key = 'cos.bf2.org/pricing-tier' and annotations.value")
		}
		dbConn = dbConn.Where(searchDbQuery.Query, searchDbQuery.Values...)
	}

	// execute query
	result := cts.connectionFactory.New().Model(&dbapi.ConnectorTypeLabel{}).
		Select("connector_type_labels.label as label, count(distinct types.id) as count").
		Joins("LEFT JOIN connector_types AS current_types on connector_type_labels.connector_type_id = current_types.id").
		Joins("LEFT JOIN (?) AS types on connector_type_labels.connector_type_id = types.id",
								dbConn.Model(&dbapi.ConnectorType{}).Select("connector_types.id")).
		Where("current_types.deleted_at is null"). // select labels for undeleted types
		Group("connector_type_labels.label").
		Order("connector_type_labels.label ASC"). // default order label name
		Find(&resourceList)
	if result.Error != nil {
		return nil, errors.ToServiceError(result.Error)
	}

	return resourceList, nil
}

func (cts *connectorTypesService) ForEachConnectorCatalogEntry(f func(id string, channel string, ccc *config.ConnectorChannelConfig) *errors.ServiceError) *errors.ServiceError {

	for _, entry := range cts.connectorsConfig.CatalogEntries {
		// create/update connector type
		connectorType, err := presenters.ConvertConnectorType(entry.ConnectorType)
		if err != nil {
			return errors.GeneralError("failed to convert connector type %s: %v", entry.ConnectorType.Id, err.Error())
		}
		if err := cts.Create(connectorType); err != nil {
			return err
		}

		// reconcile channels
		for channel, ccc := range entry.Channels {
			ccc := ccc
			err := f(entry.ConnectorType.Id, channel, &ccc)
			if err != nil {
				return err
			}
		}

		// update type checksum for latest catalog shard metadata
		dbConn := cts.connectionFactory.New()
		if err = dbConn.Model(connectorType).Where("id = ?", connectorType.ID).
			UpdateColumn("checksum", cts.connectorsConfig.CatalogChecksums[connectorType.ID]).Error; err != nil {
			return errors.GeneralError("failed to update connector type %s checksum: %v", entry.ConnectorType.Id, err.Error())
		}
	}
	return nil
}

func (cts *connectorTypesService) PutConnectorShardMetadata(connectorShardMetadata *dbapi.ConnectorShardMetadata) (int64, *errors.ServiceError) {

	var resource dbapi.ConnectorShardMetadata

	dbConn := cts.connectionFactory.New()
	dbConn = dbConn.Select("id").
		Where("connector_type_id = ?", connectorShardMetadata.ConnectorTypeId).
		Where("channel = ?", connectorShardMetadata.Channel).
		Where("revision = ?", connectorShardMetadata.Revision)

	if err := dbConn.First(&resource).Error; err != nil {
		if services.IsRecordNotFoundError(err) {
			// We need to understand if we are inserting the shard metadata with the latest revision
			// among the same connector_type_id and channel group
			var currentLatestRevision sql.NullInt64
			dbConn = cts.connectionFactory.New().
				Table("connector_shard_metadata").
				Select("max(revision)").
				Where("connector_type_id = ?", connectorShardMetadata.ConnectorTypeId).
				Where("channel = ?", connectorShardMetadata.Channel)
			if err := dbConn.Scan(&currentLatestRevision).Error; err != nil {
				return 0, errors.GeneralError("failed to find max(revision) of connector shard metadata %v: %v", connectorShardMetadata, err)
			}
			// And updating LatestRevision field accordingly
			if currentLatestRevision.Valid && connectorShardMetadata.Revision < currentLatestRevision.Int64 {
				// The shard metadata we are saving has not the latest revision,
				// so we set its LatestRevision field to currentLatestRevision
				connectorShardMetadata.LatestRevision = &currentLatestRevision.Int64
			} else {
				// The shard metadata we are saving has the latest revision,
				// so we set its LatestRevision field to nil
				connectorShardMetadata.LatestRevision = nil
			}

			// We need to create the resource...
			dbConn = cts.connectionFactory.New()
			if err := dbConn.Save(connectorShardMetadata).Error; err != nil {
				return 0, errors.GeneralError("failed to create connector shard metadata %v: %v", connectorShardMetadata, err)
			}

			// If we are inserting the latest revision we need to update other shard metadata record
			// among the same connector_type_id and channel group
			if connectorShardMetadata.LatestRevision == nil {
				// update the other records latest_revision
				dbConn = cts.connectionFactory.New().
					Table("connector_shard_metadata").
					Where("connector_type_id = ?", connectorShardMetadata.ConnectorTypeId).
					Where("channel = ?", connectorShardMetadata.Channel).
					Where("revision < ?", connectorShardMetadata.Revision)
				if err := dbConn.Update("latest_revision", connectorShardMetadata.Revision).Error; err != nil {
					return 0, errors.GeneralError("failed to update other connectors shard metadata with the latest revision from: %v", connectorShardMetadata)
				}
			}

			return resource.ID, nil
		} else {
			return 0, errors.NewWithCause(errors.ErrorGeneral, err, "unable to save connector shard metadata")
		}
	} else {
		// resource existed... update the connectorShardMetadata with the version it's been assigned in the DB...
		return resource.ID, nil
	}
}

func (cts *connectorTypesService) GetConnectorShardMetadata(typeId, channel string, revision int64) (*dbapi.ConnectorShardMetadata, *errors.ServiceError) {
	resource := &dbapi.ConnectorShardMetadata{}
	dbConn := cts.connectionFactory.New()

	err := dbConn.
		Where(dbapi.ConnectorShardMetadata{ConnectorTypeId: typeId, Channel: channel, Revision: revision}).
		First(&resource).Error

	if err != nil {
		if services.IsRecordNotFoundError(err) {
			return nil, errors.NotFound("connector type shard metadata (ConnectorTypeId: %s, Channel: %s, Revision: %v) not found.", typeId, channel, revision)
		}
		return nil, errors.GeneralError("unable to get connector type shard metadata (ConnectorTypeId: %s, Channel: %s, Revision: %v): %s", typeId, channel, revision, err)
	}
	return resource, nil
}

func (cts *connectorTypesService) GetLatestConnectorShardMetadata(typeId, channel string) (*dbapi.ConnectorShardMetadata, *errors.ServiceError) {
	resource := &dbapi.ConnectorShardMetadata{}
	dbConn := cts.connectionFactory.New()

	err := dbConn.
		Where(dbapi.ConnectorShardMetadata{ConnectorTypeId: typeId, Channel: channel}).
		Order("revision desc").
		First(&resource).Error

	if err != nil {
		if services.IsRecordNotFoundError(err) {
			return nil, errors.NotFound("connector type shard metadata not found")
		}
		return nil, errors.GeneralError("unable to get connector type shard metadata: %s", err)
	}
	return resource, nil
}

func (cts *connectorTypesService) CatalogEntriesReconciled() (bool, *errors.ServiceError) {
	var typeIds []string
	catalogChecksums := cts.connectorsConfig.CatalogChecksums
	for id := range catalogChecksums {
		typeIds = append(typeIds, id)
	}

	var connectorTypes dbapi.ConnectorTypeList
	dbConn := cts.connectionFactory.New()
	if err := dbConn.Select("id, checksum").Where("id in ?", typeIds).
		Find(&connectorTypes).Error; err != nil {
		return false, services.HandleGetError("Connector type", "id", typeIds, err)
	}

	if len(catalogChecksums) != len(connectorTypes) {
		return false, nil
	}
	for _, ct := range connectorTypes {
		if ct.Checksum == nil || *ct.Checksum != catalogChecksums[ct.ID] {
			return false, nil
		}
	}
	return true, nil
}

func (cts *connectorTypesService) DeleteOrDeprecateRemovedTypes() *errors.ServiceError {
	notToBeDeletedIDs := make([]string, len(cts.connectorsConfig.CatalogEntries))
	for _, entry := range cts.connectorsConfig.CatalogEntries {
		notToBeDeletedIDs = append(notToBeDeletedIDs, entry.ConnectorType.Id)
	}
	glog.V(5).Infof("Connector Type IDs in catalog not to be deleted: %v", notToBeDeletedIDs)

	var usedConnectorTypeIDs []string
	dbConn := cts.connectionFactory.New()
	if err := dbConn.Model(&dbapi.Connector{}).Distinct("connector_type_id").Find(&usedConnectorTypeIDs).Error; err != nil {
		return errors.GeneralError("failed to find active connectors: %v", err.Error())
	}
	glog.V(5).Infof("Connector Type IDs used by at least an active connector not to be deleted: %v", usedConnectorTypeIDs)

	// flag deprecated types
	deprecatedIds := getDeprecatedTypes(usedConnectorTypeIDs, notToBeDeletedIDs)
	if len(deprecatedIds) > 0 {
		if err := dbConn.Model(&dbapi.ConnectorType{}).
			Where("id IN ?", deprecatedIds).
			Update("deprecated", true).Error; err != nil {
			return errors.GeneralError("failed to deprecate connector types with ids %v : %v", deprecatedIds, err.Error())
		}
		glog.V(5).Infof("Deprecated Connector Types with id IN: %v", deprecatedIds)
	}

	notToBeDeletedIDs = append(notToBeDeletedIDs, usedConnectorTypeIDs...)

	if err := dbConn.Delete(&dbapi.ConnectorType{}, "id NOT IN ?", notToBeDeletedIDs).Error; err != nil {
		return errors.GeneralError("failed to delete connector type with ids %v : %v", notToBeDeletedIDs, err.Error())
	}
	glog.V(5).Infof("Deleted Connector Type with id NOT IN: %v", notToBeDeletedIDs)
	return nil
}

// get ids that are used, but not in the latest catalog, i.e. deprecated
func getDeprecatedTypes(usedConnectorTypeIDs []string, notToBeDeletedIDs []string) []string {
	latestIds := make(map[string]struct{})
	arrays.ForEach(notToBeDeletedIDs, func(k string) {
		latestIds[k] = sets.Empty{}
	})
	deprecatedIds := make([]string, 0)
	arrays.ForEach(usedConnectorTypeIDs, func(k string) {
		if _, ok := latestIds[k]; !ok {
			deprecatedIds = append(deprecatedIds, k)
		}
	})
	return deprecatedIds
}

func (cts *connectorTypesService) ListCatalogEntries(listArgs *coreService.ListArguments) ([]dbapi.ConnectorCatalogEntry, *api.PagingMeta, *errors.ServiceError) {
	types, pagin, err := cts.List(listArgs)
	if err != nil {
		return nil, nil, err
	}

	entries := make([]dbapi.ConnectorCatalogEntry, len(types))

	for i := range types {
		ct := types[i]
		entry, err := cts.toConnectorCatalogEntry(ct)
		if err != nil {
			return nil, nil, err
		}

		entries[i] = *entry
	}

	return entries, pagin, nil
}

func (cts *connectorTypesService) GetCatalogEntry(id string) (*dbapi.ConnectorCatalogEntry, *errors.ServiceError) {
	ct, err := cts.Get(id)
	if err != nil {
		return nil, err
	}

	return cts.toConnectorCatalogEntry(ct)
}

func (cts *connectorTypesService) toConnectorCatalogEntry(ct *dbapi.ConnectorType) (*dbapi.ConnectorCatalogEntry, *errors.ServiceError) {
	entry := dbapi.ConnectorCatalogEntry{
		ConnectorType: ct,
		Channels:      make(map[string]*dbapi.ConnectorShardMetadata),
	}

	for _, c := range ct.Channels {
		meta, err := cts.GetLatestConnectorShardMetadata(ct.ID, c.Channel)
		if err != nil {
			return nil, err
		}

		entry.Channels[c.Channel] = meta
	}

	return &entry, nil
}
