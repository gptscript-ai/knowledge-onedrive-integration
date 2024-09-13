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
	FileName    string `json:"fileName"`
	DisplayName string `json:"displayName"`
	URL         string `json:"url"`
	UpdatedAt   string `json:"updatedAt"`
	Sync        bool   `json:"sync"`
}

func main() {
	cred := NewStaticTokenCredential(os.Getenv("GPTSCRIPT_GRAPH_MICROSOFT_COM_BEARER_TOKEN"))
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, []string{})
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
	ctx := context.Background()

	metadata := map[string]FileDetails{}
	externalLinks := map[string]string{}
	dataPath := path.Join(os.Getenv("WORKSPACE_DIR"), "knowledge", "integrations", "onedrive")
	metadataPath := path.Join(dataPath, "metadata.json")
	externalLinkPath := path.Join(dataPath, "externalLinks.json")
	if _, err := os.Stat(dataPath); os.IsNotExist(err) {
		err := os.MkdirAll(dataPath, 0755)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
	} else {
		if _, err := os.Stat(metadataPath); err == nil {
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
		}

		if _, err := os.Stat(externalLinkPath); err == nil {
			data, err := os.ReadFile(externalLinkPath)
			if err != nil {
				logrus.Error(err)
				os.Exit(1)
			}

			err = json.Unmarshal(data, &externalLinks)
			if err != nil {
				logrus.Error(err)
				os.Exit(1)
			}
		}
	}

	items := map[string]models.DriveItemable{}
	for link := range externalLinks {
		requestParameters := &shares.ItemDriveItemRequestBuilderGetQueryParameters{
			Expand: []string{"children"},
		}
		configuration := &shares.ItemDriveItemRequestBuilderGetRequestConfiguration{
			QueryParameters: requestParameters,
		}
		shareDriveItem, err := client.Shares().BySharedDriveItemId(encodeURL(link)).DriveItem().Get(ctx, configuration)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}

		children, err := getChildrenFileForItem(ctx, client, shareDriveItem)
		if err != nil {
			logrus.Error(err)
			os.Exit(1)
		}
		for _, child := range children {
			items[*child.GetId()] = child
		}
	}

	if err := saveToMetadata(ctx, metadata, client, dataPath, items); err != nil {
		logrus.Error(err)
		os.Exit(1)
	}

	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
	err = os.WriteFile(metadataPath, data, 0644)
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
	logrus.Info(fmt.Sprintf("Saved metadata to %s", metadataPath))
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

func saveToMetadata(ctx context.Context, metadata map[string]FileDetails, client *msgraphsdk.GraphServiceClient, dataPath string, items map[string]models.DriveItemable) error {
	for _, item := range items {
		if detail, ok := metadata[*item.GetId()]; ok {
			if detail.Sync {
				downloadPath := path.Join(dataPath, *item.GetId(), detail.FileName)
				if _, err := os.Stat(path.Join(dataPath, *item.GetId())); err != nil {
					err := os.MkdirAll(path.Join(dataPath, *item.GetId()), 0755)
					if err != nil {
						return err
					}
				}
				if _, err := os.Stat(downloadPath); err != nil || detail.UpdatedAt != (*item.GetLastModifiedDateTime()).String() {
					{
						data, err := client.Drives().ByDriveId(*item.GetParentReference().GetDriveId()).Items().ByDriveItemId(*item.GetId()).Content().Get(ctx, nil)
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
			}
			detail.DisplayName = getDisplayName(item)
			detail.FileName = *item.GetName()
			detail.URL = *item.GetWebUrl()
			detail.UpdatedAt = (*item.GetLastModifiedDateTime()).String()
			metadata[*item.GetId()] = detail
		} else {
			metadata[*item.GetId()] = FileDetails{
				FileName:    *item.GetName(),
				DisplayName: getDisplayName(item),
				URL:         *item.GetWebUrl(),
				UpdatedAt:   (*item.GetLastModifiedDateTime()).String(),
			}
		}
	}

	for id := range metadata {
		if _, ok := items[id]; !ok {
			delete(metadata, id)
		}
	}
	return nil
}

func getDisplayName(item models.DriveItemable) string {
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
