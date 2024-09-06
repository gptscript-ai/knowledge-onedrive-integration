package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	msgraphsdk "github.com/microsoftgraph/msgraph-sdk-go"
	drives2 "github.com/microsoftgraph/msgraph-sdk-go/drives"
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
	Name    string
	URL     string
	Updated string
	Sync    bool
}

func main() {
	cred := NewStaticTokenCredential(os.Getenv("GPTSCRIPT_GRAPH_MICROSOFT_COM_BEARER_TOKEN"))
	client, err := msgraphsdk.NewGraphServiceClientWithCredentials(cred, []string{})
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
	ctx := context.Background()
	drive, err := client.Me().Drive().Get(ctx, nil)
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}

	metadata := map[string]FileDetails{}
	dataPath := path.Join(os.Getenv("WORKSPACE_DIR"), "knowledge", "integrations", "onedrive")
	metadataPath := path.Join(dataPath, "metadata.json")
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
	}

	config := drives2.ItemItemsRequestBuilderGetRequestConfiguration{
		QueryParameters: &drives2.ItemItemsRequestBuilderGetQueryParameters{
			Filter: to.Ptr("file ne null"),
		},
	}
	driveItems, err := client.Drives().ByDriveId(*drive.GetId()).Items().Get(ctx, &config)
	if err != nil {
		logrus.Error(err)
		os.Exit(1)
	}
	for _, item := range driveItems.GetValue() {
		if detail, ok := metadata[*item.GetId()]; ok {
			if detail.Sync {
				downloadPath := path.Join(dataPath, *item.GetId(), detail.Name)
				if _, err := os.Stat(path.Join(dataPath, *item.GetId())); err != nil {
					err := os.MkdirAll(path.Join(dataPath, *item.GetId()), 0755)
					if err != nil {
						logrus.Error(err)
						os.Exit(1)
					}
				}
				if _, err := os.Stat(downloadPath); err != nil || detail.Updated != (*item.GetLastModifiedDateTime()).String() {
					{
						data, err := client.Drives().ByDriveId(*drive.GetId()).Items().ByDriveItemId(*item.GetId()).Content().Get(ctx, nil)
						if err != nil {
							logrus.Error(err)
							os.Exit(1)
						}

						err = os.WriteFile(downloadPath, data, 0644)
						if err != nil {
							logrus.Error(err)
							os.Exit(1)
						}
						logrus.Info(fmt.Sprintf("Downloaded %s", downloadPath))
					}
				}
			}
			detail.Name = *item.GetName()
			detail.URL = *item.GetWebUrl()
			detail.Updated = (*item.GetLastModifiedDateTime()).String()
			metadata[*item.GetId()] = detail
		} else {
			metadata[*item.GetId()] = FileDetails{
				Name:    *item.GetName(),
				URL:     *item.GetWebUrl(),
				Updated: (*item.GetLastModifiedDateTime()).String(),
			}
		}
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
