package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/content-services/content-sources-backend/pkg/api"
	"github.com/content-services/content-sources-backend/pkg/config"
	"github.com/content-services/content-sources-backend/pkg/dao"
	"github.com/content-services/content-sources-backend/pkg/db"
	"github.com/content-services/content-sources-backend/pkg/external_repos"
	"github.com/content-services/content-sources-backend/pkg/models"
	"github.com/content-services/content-sources-backend/pkg/pulp_client"
	"github.com/content-services/content-sources-backend/pkg/tasks/payloads"
	"github.com/content-services/content-sources-backend/pkg/tasks/queue"
	zest "github.com/content-services/zest/release/v2023"
	"github.com/google/uuid"
	"github.com/openlyinc/pointy"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func SnapshotHandler(ctx context.Context, task *models.TaskInfo, queue *queue.Queue) error {
	opts := payloads.SnapshotPayload{}
	if err := json.Unmarshal(task.Payload, &opts); err != nil {
		return fmt.Errorf("payload incorrect type for Snapshot")
	}
	logger := LogForTask(task.Id.String(), task.Typename, task.RequestID)
	ctxWithLogger := logger.WithContext(ctx)

	daoReg := dao.GetDaoRegistry(db.DB)
	domainName, err := daoReg.Domain.FetchOrCreateDomain(task.OrgId)
	if err != nil {
		return err
	}
	pulpClient := pulp_client.GetPulpClientWithDomain(ctxWithLogger, domainName)

	sr := SnapshotRepository{
		orgId:          task.OrgId,
		domainName:     domainName,
		repositoryUUID: task.RepositoryUUID,
		daoReg:         daoReg,
		pulpClient:     pulpClient,
		task:           task,
		payload:        &opts,
		queue:          queue,
		ctx:            ctx,
		logger:         logger,
	}
	return sr.Run()
}

type SnapshotRepository struct {
	orgId          string
	domainName     string
	repositoryUUID uuid.UUID
	snapshotUUID   string
	daoReg         *dao.DaoRegistry
	pulpClient     pulp_client.PulpClient
	payload        *payloads.SnapshotPayload
	task           *models.TaskInfo
	queue          *queue.Queue
	ctx            context.Context
	logger         *zerolog.Logger
}

// SnapshotRepository creates a snapshot of a given repository config
func (sr *SnapshotRepository) Run() (err error) {
	defer func() {
		if errors.Is(err, context.Canceled) {
			cleanupErr := sr.cleanupOnCancel()
			if cleanupErr != nil {
				sr.logger.Err(cleanupErr).Msg("error cleaning up canceled snapshot")
			}
		}
	}()

	var remoteHref string
	var repoHref string
	var publicationHref string
	_, err = sr.pulpClient.LookupOrCreateDomain(sr.domainName)
	if err != nil {
		return err
	}
	err = sr.pulpClient.UpdateDomainIfNeeded(sr.domainName)
	if err != nil {
		return err
	}
	repoConfig, err := sr.lookupRepoObjects()
	if err != nil {
		return err
	}

	repoConfigUuid := repoConfig.UUID

	remoteHref, err = sr.findOrCreateRemote(repoConfig)
	if err != nil {
		return err
	}

	repoHref, err = sr.findOrCreatePulpRepo(repoConfigUuid, remoteHref)
	if err != nil {
		return err
	}

	versionHref, err := sr.syncRepository(repoHref)
	if err != nil {
		return err
	}
	if versionHref == nil {
		// Nothing updated, no snapshot needed
		// TODO: figure out how to better indicate this to the user
		return nil
	}

	publicationHref, err = sr.findOrCreatePublication(versionHref)
	if err != nil {
		return err
	}

	if sr.payload.SnapshotIdent == nil {
		ident := uuid.NewString()
		sr.payload.SnapshotIdent = &ident
	}
	distHref, distPath, err := sr.createDistribution(publicationHref, repoConfig.UUID, *sr.payload.SnapshotIdent)
	if err != nil {
		return err
	}
	version, err := sr.pulpClient.GetRpmRepositoryVersion(*versionHref)
	if err != nil {
		return err
	}

	if version.ContentSummary == nil {
		sr.logger.Error().Msgf("Found nil content Summary for version %v", *versionHref)
	}

	current, added, removed := ContentSummaryToContentCounts(version.ContentSummary)

	snap := models.Snapshot{
		VersionHref:                 *versionHref,
		PublicationHref:             publicationHref,
		DistributionPath:            distPath,
		RepositoryPath:              filepath.Join(sr.domainName, distPath),
		DistributionHref:            distHref,
		RepositoryConfigurationUUID: repoConfigUuid,
		ContentCounts:               current,
		AddedCounts:                 added,
		RemovedCounts:               removed,
	}
	sr.logger.Debug().Msgf("Snapshot created at: %v", distPath)
	err = sr.daoReg.Snapshot.Create(&snap)
	if err != nil {
		return err
	}
	sr.snapshotUUID = snap.UUID
	return nil
}

func (sr *SnapshotRepository) createDistribution(publicationHref string, repoConfigUUID string, snapshotId string) (string, string, error) {
	distPath := fmt.Sprintf("%v/%v", repoConfigUUID, snapshotId)

	foundDist, err := sr.pulpClient.FindDistributionByPath(distPath)
	if err != nil && foundDist != nil {
		return *foundDist.PulpHref, distPath, nil
	} else if err != nil {
		sr.logger.Error().Err(err).Msgf("Error looking up distribution by path %v", distPath)
	}

	if sr.payload.DistributionTaskHref == nil {
		distTaskHref, err := sr.pulpClient.CreateRpmDistribution(publicationHref, snapshotId, distPath)
		if err != nil {
			return "", "", err
		}
		sr.payload.DistributionTaskHref = distTaskHref
	}

	distTask, err := sr.pulpClient.PollTask(*sr.payload.DistributionTaskHref)
	if err != nil {
		return "", "", err
	}
	distHref := pulp_client.SelectRpmDistributionHref(distTask)
	if distHref == nil {
		return "", "", fmt.Errorf("Could not find a distribution href in task: %v", distTask.PulpHref)
	}
	return *distHref, distPath, nil
}

func (sr *SnapshotRepository) findOrCreatePublication(versionHref *string) (string, error) {
	var publicationHref *string
	// Publication
	publication, err := sr.pulpClient.FindRpmPublicationByVersion(*versionHref)
	if err != nil {
		return "", err
	}
	if publication == nil || publication.PulpHref == nil {
		if sr.payload.PublicationTaskHref == nil {
			publicationTaskHref, err := sr.pulpClient.CreateRpmPublication(*versionHref)
			if err != nil {
				return "", err
			}
			sr.payload.PublicationTaskHref = publicationTaskHref
			err = sr.UpdatePayload()
			if err != nil {
				return "", err
			}
		} else {
			sr.logger.Debug().Str("pulp_task_id", *sr.payload.PublicationTaskHref).Msg("Resuming Publication task")
		}

		publicationTask, err := sr.pulpClient.PollTask(*sr.payload.PublicationTaskHref)
		if err != nil {
			return "", err
		}
		publicationHref = pulp_client.SelectPublicationHref(publicationTask)
		if publicationHref == nil {
			return "", fmt.Errorf("Could not find a publication href in task: %v", publicationTask.PulpHref)
		}
	} else {
		publicationHref = publication.PulpHref
	}
	return *publicationHref, nil
}

func (sr *SnapshotRepository) UpdatePayload() error {
	var err error
	a := *sr.payload
	sr.task, err = (*sr.queue).UpdatePayload(sr.task, a)
	if err != nil {
		return err
	}
	return nil
}

func (sr *SnapshotRepository) syncRepository(repoHref string) (*string, error) {
	if sr.payload.SyncTaskHref == nil {
		syncTaskHref, err := sr.pulpClient.SyncRpmRepository(repoHref, nil)
		if err != nil {
			return nil, err
		}
		sr.payload.SyncTaskHref = &syncTaskHref
		err = sr.UpdatePayload()
		if err != nil {
			return nil, err
		}
	} else {
		sr.logger.Debug().Str("pulp_task_id", *sr.payload.SyncTaskHref).Msg("Resuming Sync task")
	}

	syncTask, err := sr.pulpClient.PollTask(*sr.payload.SyncTaskHref)
	if err != nil {
		return nil, err
	}

	versionHref := pulp_client.SelectVersionHref(syncTask)
	return versionHref, nil
}

func (sr *SnapshotRepository) findOrCreatePulpRepo(repoConfigUUID string, remoteHref string) (string, error) {
	repoResp, err := sr.pulpClient.GetRpmRepositoryByName(repoConfigUUID)
	if err != nil {
		return "", err
	}
	if repoResp == nil {
		repoResp, err = sr.pulpClient.CreateRpmRepository(repoConfigUUID, &remoteHref)
		if err != nil {
			return "", err
		}
	}
	return *repoResp.PulpHref, nil
}

func urlIsRedHat(url string) bool {
	return strings.Contains(url, "cdn.redhat.com")
}

func (sr *SnapshotRepository) findOrCreateRemote(repoConfig api.RepositoryResponse) (string, error) {
	var clientCertPair *string
	var caCert *string
	if repoConfig.OrgID == config.RedHatOrg && urlIsRedHat(repoConfig.URL) {
		clientCertPair = config.Get().Certs.CdnCertPairString
		ca, err := external_repos.LoadCA()
		if err != nil {
			log.Err(err).Msg("Cannot load red hat ca file")
		}
		caCert = pointy.Pointer(string(ca))
	}

	remoteResp, err := sr.pulpClient.GetRpmRemoteByName(repoConfig.UUID)
	if err != nil {
		return "", err
	}
	if remoteResp == nil {
		remoteResp, err = sr.pulpClient.CreateRpmRemote(repoConfig.UUID, repoConfig.URL, clientCertPair, clientCertPair, caCert)
		if err != nil {
			return "", err
		}
	} else if remoteResp.PulpHref != nil { // blindly update the remote
		_, err = sr.pulpClient.UpdateRpmRemote(*remoteResp.PulpHref, repoConfig.URL, clientCertPair, clientCertPair, caCert)
		if err != nil {
			return "", err
		}
	}
	return *remoteResp.PulpHref, nil
}

func (sr *SnapshotRepository) lookupRepoObjects() (api.RepositoryResponse, error) {
	repoConfig, err := sr.daoReg.RepositoryConfig.FetchByRepoUuid(sr.orgId, sr.repositoryUUID.String())
	if err != nil {
		return api.RepositoryResponse{}, err
	}
	return repoConfig, nil
}

func (sr *SnapshotRepository) cleanupOnCancel() error {
	logger := LogForTask(sr.task.Id.String(), sr.task.Typename, sr.task.RequestID)
	// TODO In Go 1.21 we could use context.WithoutCancel() to make copy of parent ctx that isn't canceled
	ctxWithLogger := logger.WithContext(context.Background())
	pulpClient := pulp_client.GetPulpClientWithDomain(ctxWithLogger, sr.domainName)
	if sr.payload.SyncTaskHref != nil {
		task, err := pulpClient.CancelTask(*sr.payload.SyncTaskHref)
		if err != nil {
			return err
		}
		task, err = pulpClient.GetTask(*sr.payload.SyncTaskHref)
		if err != nil {
			return err
		}
		if sr.payload.PublicationTaskHref != nil {
			_, err := pulpClient.CancelTask(*sr.payload.PublicationTaskHref)
			if err != nil {
				return err
			}
		}
		versionHref := pulp_client.SelectVersionHref(&task)
		if versionHref != nil {
			_, err = pulpClient.DeleteRpmRepositoryVersion(*versionHref)
			if err != nil {
				return err
			}
		}
	}
	if sr.payload.DistributionTaskHref != nil {
		task, err := pulpClient.CancelTask(*sr.payload.DistributionTaskHref)
		if err != nil {
			return err
		}
		task, err = pulpClient.GetTask(*sr.payload.DistributionTaskHref)
		if err != nil {
			return err
		}
		versionHref := pulp_client.SelectRpmDistributionHref(&task)
		if versionHref != nil {
			_, err = pulpClient.DeleteRpmDistribution(*versionHref)
			if err != nil {
				return err
			}
		}
	}
	if sr.snapshotUUID != "" {
		err := sr.daoReg.Snapshot.Delete(sr.snapshotUUID)
		if err != nil {
			return err
		}
	}
	return nil
}

func ContentSummaryToContentCounts(summary *zest.RepositoryVersionResponseContentSummary) (models.ContentCountsType, models.ContentCountsType, models.ContentCountsType) {
	presentCount := models.ContentCountsType{}
	addedCount := models.ContentCountsType{}
	removedCount := models.ContentCountsType{}
	if summary != nil {
		for contentType, item := range summary.Present {
			num, ok := item["count"].(float64)
			if ok {
				presentCount[contentType] = int64(num)
			}
		}
		for contentType, item := range summary.Added {
			num, ok := item["count"].(float64)
			if ok {
				addedCount[contentType] = int64(num)
			}
		}
		for contentType, item := range summary.Removed {
			num, ok := item["count"].(float64)
			if ok {
				removedCount[contentType] = int64(num)
			}
		}
	}
	return presentCount, addedCount, removedCount
}
