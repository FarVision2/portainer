package stacks

import (
	"net/http"
	"os"
	"strconv"

	portainer "github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/filesystem"
	gittypes "github.com/portainer/portainer/api/git/types"
	"github.com/portainer/portainer/api/git/update"
	"github.com/portainer/portainer/api/http/security"
	"github.com/portainer/portainer/api/internal/registryutils"
	k "github.com/portainer/portainer/api/kubernetes"
	"github.com/portainer/portainer/api/stacks/deployments"
	httperror "github.com/portainer/portainer/pkg/libhttp/error"
	"github.com/portainer/portainer/pkg/libhttp/request"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

type kubernetesFileStackUpdatePayload struct {
	StackFileContent string
	// Name of the stack
	StackName string
}

type kubernetesGitStackUpdatePayload struct {
	RepositoryReferenceName  string
	RepositoryAuthentication bool
	RepositoryUsername       string
	RepositoryPassword       string
	AutoUpdate               *portainer.AutoUpdateSettings
	TLSSkipVerify            bool
}

func (payload *kubernetesFileStackUpdatePayload) Validate(r *http.Request) error {
	if len(payload.StackFileContent) == 0 {
		return errors.New("Invalid stack file content")
	}

	return nil
}

func (payload *kubernetesGitStackUpdatePayload) Validate(r *http.Request) error {
	if err := update.ValidateAutoUpdateSettings(payload.AutoUpdate); err != nil {
		return err
	}

	return nil
}

func (handler *Handler) updateKubernetesStack(r *http.Request, stack *portainer.Stack, endpoint *portainer.Endpoint) *httperror.HandlerError {
	if stack.GitConfig != nil {
		// Stop the autoupdate job if there is any
		if stack.AutoUpdate != nil {
			deployments.StopAutoupdate(stack.ID, stack.AutoUpdate.JobID, handler.Scheduler)
		}

		var payload kubernetesGitStackUpdatePayload

		if err := request.DecodeAndValidateJSONPayload(r, &payload); err != nil {
			return httperror.BadRequest("Invalid request payload", err)
		}

		stack.GitConfig.ReferenceName = payload.RepositoryReferenceName
		stack.GitConfig.TLSSkipVerify = payload.TLSSkipVerify
		stack.GitConfig.Authentication = nil
		stack.AutoUpdate = payload.AutoUpdate

		if payload.RepositoryAuthentication {
			password := payload.RepositoryPassword
			if password == "" && stack.GitConfig != nil && stack.GitConfig.Authentication != nil {
				password = stack.GitConfig.Authentication.Password
			}

			stack.GitConfig.Authentication = &gittypes.GitAuthentication{
				Username: payload.RepositoryUsername,
				Password: password,
			}

			if _, err := handler.GitService.LatestCommitID(stack.GitConfig.URL, stack.GitConfig.ReferenceName, stack.GitConfig.Authentication.Username, stack.GitConfig.Authentication.Password, stack.GitConfig.TLSSkipVerify); err != nil {
				return httperror.InternalServerError("Unable to fetch git repository", err)
			}
		}

		if payload.AutoUpdate != nil && payload.AutoUpdate.Interval != "" {
			jobID, e := deployments.StartAutoupdate(stack.ID, stack.AutoUpdate.Interval, handler.Scheduler, handler.StackDeployer, handler.DataStore, handler.GitService)
			if e != nil {
				return e
			}
			stack.AutoUpdate.JobID = jobID
		}

		return nil
	}

	var payload kubernetesFileStackUpdatePayload

	if err := request.DecodeAndValidateJSONPayload(r, &payload); err != nil {
		return httperror.BadRequest("Invalid request payload", err)
	}

	tokenData, err := security.RetrieveTokenData(r)
	if err != nil {
		return httperror.BadRequest("Failed to retrieve user token data", err)
	}

	tempFileDir, _ := os.MkdirTemp("", "kub_file_content")
	defer os.RemoveAll(tempFileDir)

	if err := filesystem.WriteToFile(filesystem.JoinPaths(tempFileDir, stack.EntryPoint), []byte(payload.StackFileContent)); err != nil {
		return httperror.InternalServerError("Failed to persist deployment file in a temp directory", err)
	}

	if payload.StackName != stack.Name {
		stack.Name = payload.StackName
		if err := handler.DataStore.Stack().Update(stack.ID, stack); err != nil {
			return httperror.InternalServerError("Failed to update stack name", err)
		}
	}

	// Refresh ECR registry secret if needed
	// RefreshEcrSecret method checks if the namespace has any ECR registry
	// otherwise return nil
	cli, err := handler.KubernetesClientFactory.GetKubeClient(endpoint)
	if err == nil {
		registryutils.RefreshEcrSecret(cli, endpoint, handler.DataStore, stack.Namespace)
	}

	// Use temp dir as the stack project path for deployment
	// so if the deployment failed, the original file won't be over-written
	stack.ProjectPath = tempFileDir

	if _, err := handler.deployKubernetesStack(tokenData.ID, endpoint, stack, k.KubeAppLabels{
		StackID:   int(stack.ID),
		StackName: stack.Name,
		Owner:     stack.CreatedBy,
		Kind:      "content",
	}); err != nil {
		return httperror.InternalServerError("Unable to deploy Kubernetes stack via file content", err)
	}

	stackFolder := strconv.Itoa(int(stack.ID))
	projectPath, err := handler.FileService.UpdateStoreStackFileFromBytes(stackFolder, stack.EntryPoint, []byte(payload.StackFileContent))
	if err != nil {
		if rollbackErr := handler.FileService.RollbackStackFile(stackFolder, stack.EntryPoint); rollbackErr != nil {
			log.Warn().Err(rollbackErr).Msg("rollback stack file error")
		}

		return httperror.InternalServerError("Unable to persist Kubernetes Manifest file on disk", err)
	}
	stack.ProjectPath = projectPath

	handler.FileService.RemoveStackFileBackup(stackFolder, stack.EntryPoint)

	return nil
}
