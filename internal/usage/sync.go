package usage

import (
	"github.com/infracost/infracost/internal/schema"
)

type SyncResult struct {
	ResourceCount    int
	EstimationCount  int
	EstimationErrors map[string]error
}

func SyncUsageData(projects []*schema.Project, existingUsageData map[string][]*schema.UsageItem, path string) (*SyncResult, error) {
	if path == "" {
		return nil, nil
	}

	referenceFile, err := LoadReferenceFile()
	if err != nil {
		return nil, err
	}

	// TODO: update this when we properly support multiple projects in usage
	resources := make([]*schema.Resource, 0)
	for _, project := range projects {
		resources = append(resources, project.Resources...)
	}

	syncResult, syncedResourceUsages := syncResourceUsages(resources, referenceFile, existingUsageData)

	usageFile := UsageFile{
		Version: maxUsageFileVersion,
	}
	if syncedResourceUsages != nil {
		usageFile.ResourceUsages = syncedResourceUsages
	}
	
	err = usageFile.WriteToPath(path)
	if err != nil {
		return nil, err
	}

	return &syncResult, nil
}
