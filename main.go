package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	drives2 "github.com/microsoftgraph/msgraph-sdk-go/drives"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	"github.com/microsoftgraph/msgraph-sdk-go/shares"
	"github.com/sirupsen/logrus"
)

type Metadata struct {
	Input  MetadataInput  `json:"input"`
	Output MetadataOutput `json:"output"`
}

type MetadataInput struct {
	SharedLinks []string `json:"sharedLinks"`
	OutputDir   string   `json:"outputDir"`
}

type MetadataOutput struct {
	Status  string                 `json:"status"`
	Error   string                 `json:"error"`
	Files   map[string]FileDetails `json:"files"`
	Folders map[string]struct{}    `json:"folders"`
}

type StaticTokenCredential struct {
	token string
}

func NewStaticTokenCredential(token string) StaticTokenCredential {
	return StaticTokenCredential{
		token: token,
	}
}

func (s StaticTokenCredential) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{
		Token: s.token,
	}, nil
}

type FileDetails struct {
	FilePath  string `json:"filePath"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updatedAt"`
}

func main() {
	cred := NewStaticTokenCredential(os.Getenv("GPTSCRIPT_GRAPH_MICROSOFT_COM_BEARER_TOKEN"))
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, []string{})
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
	ctx := context.Background()
	workingDir := os.Getenv("GPTSCRIPT_WORKSPACE_DIR")
	if workingDir == "" {
		workingDir, err = os.Getwd()
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
	}

	metadata := Metadata{}
	metadataPath := path.Join(workingDir, ".metadata.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		logrus.Error("metadata.json not found")
		os.Exit(1)
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}

	err = json.Unmarshal(data, &metadata)
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}

	if metadata.Output.Files == nil {
		metadata.Output.Files = make(map[string]FileDetails)
	}

	if metadata.Input.OutputDir != "" {
		workingDir = metadata.Input.OutputDir
	}

	if metadata.Output.Folders == nil {
		metadata.Output.Folders = make(map[string]struct{})
	}

	if err := sync(ctx, metadata, client, workingDir, metadataPath); err != nil {
		metadata.Output.Error = err.Error()
		if err := writeMetadata(metadata, metadataPath); err != nil {
			logrus.Error(err)
		}
		os.Exit(1)
	}

	metadata.Output.Status = "Done"
	metadata.Output.Error = ""
	if err := writeMetadata(metadata, metadataPath); err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
}

func sync(ctx context.Context, metadata Metadata, client *msgraphsdk.GraphServiceClient, workingDir string, metadataPath string) error {
	items := make(map[string]struct {
		Item models.DriveItemable
		Root string
	})
	for _, link := range metadata.Input.SharedLinks {
		requestParameters := &shares.ItemDriveItemRequestBuilderGetQueryParameters{
			Expand: []string{"children"},
		}
		configuration := &shares.ItemDriveItemRequestBuilderGetRequestConfiguration{
			QueryParameters: requestParameters,
		}
		shareDriveItem, err := client.Shares().BySharedDriveItemId(encodeURL(link)).DriveItem().Get(ctx, configuration)
		if err != nil {
			return err
		}
		root := path.Dir(getFullName(shareDriveItem))

		children, err := getChildrenFileForItem(ctx, client, shareDriveItem)
		if err != nil {
			return err
		}
		for _, child := range children {
			items[*child.GetId()] = struct {
				Item models.DriveItemable
				Root string
			}{
				Item: child,
				Root: root,
			}
		}
	}
	if err := saveToMetadata(ctx, metadata, client, workingDir, metadataPath, items); err != nil {
		return err
	}

	return nil
}

func writeMetadata(metadata Metadata, path string) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func getChildrenFileForItem(ctx context.Context, client *msgraphsdk.GraphServiceClient, item models.DriveItemable) ([]models.DriveItemable, error) {
	if item.GetFile() != nil {
		return []models.DriveItemable{item}, nil
	}

	var result []models.DriveItemable
	for _, child := range item.GetChildren() {
		item, err := client.Drives().ByDriveId(*child.GetParentReference().GetDriveId()).Items().ByDriveItemId(*child.GetId()).Get(ctx, &drives2.ItemItemsDriveItemItemRequestBuilderGetRequestConfiguration{
			QueryParameters: &drives2.ItemItemsDriveItemItemRequestBuilderGetQueryParameters{
				Expand: []string{"children"},
			},
		})
		if err != nil {
			return nil, err
		}
		children, err := getChildrenFileForItem(ctx, client, item)
		if err != nil {
			return nil, err
		}
		result = append(result, children...)
	}
	return result, nil
}

func saveToMetadata(ctx context.Context, metadata Metadata, client *msgraphsdk.GraphServiceClient, dataPath string, metadataPath string, items map[string]struct {
	Item models.DriveItemable
	Root string
}) error {
	folders := make(map[string]struct{})
	for _, item := range items {
		fullPath := getFullName(item.Item)
		relativePath := strings.TrimPrefix(fullPath, item.Root)
		downloadPath := path.Join(dataPath, relativePath)
		topRootFolder := strings.Split(strings.TrimPrefix(relativePath, string(os.PathSeparator)), string(os.PathSeparator))[0]
		detail, ok := metadata.Output.Files[*item.Item.GetId()]
		if !ok {
			detail.FilePath = downloadPath
			detail.URL = *item.Item.GetWebUrl()
			detail.UpdatedAt = (*item.Item.GetLastModifiedDateTime()).String()
			metadata.Output.Files[*item.Item.GetId()] = detail
		}
		if _, err := os.Stat(path.Dir(downloadPath)); err != nil {
			err := os.MkdirAll(path.Dir(downloadPath), 0755)
			if err != nil {
				return err
			}
		}
		if _, err := os.Stat(downloadPath); err != nil || detail.UpdatedAt != item.Item.GetLastModifiedDateTime().String() {
			{
				driveID := *item.Item.GetParentReference().GetDriveId()
				data, err := client.Drives().ByDriveId(driveID).Items().ByDriveItemId(*item.Item.GetId()).Content().Get(ctx, nil)
				if err != nil {
					return err
				}

				err = os.WriteFile(downloadPath, data, 0644)
				if err != nil {
					return err
				}
				logrus.Info(fmt.Sprintf("Downloaded %s", downloadPath))
			}
		}
		folders[topRootFolder] = struct{}{}
		metadata.Output.Folders[topRootFolder] = struct{}{}
		metadata.Output.Status = fmt.Sprintf("Synced %d files out of %d", len(metadata.Output.Files), len(items))
		if err := writeMetadata(metadata, metadataPath); err != nil {
			return err
		}
	}
	for id := range metadata.Output.Files {
		found := false
		if _, ok := items[id]; ok {
			found = true
		}
		if !found {
			if metadata.Output.Files[id].FilePath != "" {
				logrus.Infof("Deleting %s", metadata.Output.Files[id].FilePath)
				downloadPath := path.Join(dataPath, metadata.Output.Files[id].FilePath)
				if err := os.RemoveAll(downloadPath); err != nil {
					return err
				}
			}
			delete(metadata.Output.Files, id)
		}
	}

	for folder := range metadata.Output.Folders {
		if _, ok := folders[folder]; !ok {
			logrus.Infof("Deleting folder %s", folder)
			if err := os.RemoveAll(strings.TrimRight(folder, "/")); err != nil {
				return err
			}
			delete(metadata.Output.Folders, folder)
		}
	}

	return nil
}

func getFullName(item models.DriveItemable) string {
	p := item.GetParentReference().GetPath()
	if p != nil {
		_, after, found := strings.Cut(*p, ":")
		if found {
			return path.Join(after, *item.GetName())
		}
	}
	return ""
}

func encodeURL(u string) string {
	base64Value := base64.StdEncoding.EncodeToString([]byte(u))

	encodedUrl := "u!" + strings.TrimRight(base64Value, "=")
	encodedUrl = strings.ReplaceAll(encodedUrl, "/", "_")
	encodedUrl = strings.ReplaceAll(encodedUrl, "+", "-")
	return encodedUrl
}
