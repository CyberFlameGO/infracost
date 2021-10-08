package usage

import (
	"strings"

	"github.com/infracost/infracost"
	"github.com/pkg/errors"
)

type UsageFileReference struct {
	usageFile *UsageFile
}

func LoadReferenceFile() (*UsageFile, error) {
	contents := infracost.GetReferenceUsageFileContents()
	if contents == nil {
		return &UsageFile{}, errors.New("Could not find reference usage file")
	}

	usageFile, err := LoadUsageFileFromString(string(*contents))
	if err != nil {
		return usageFile, err
	}
	
	for _, resourceUsage := range usageFile.ResourceUsages {
		resourceType := strings.Split(resourceUsage.Name, ".")[0]
		resourceUsage.Name = resourceType
	}
	
	return usageFile, nil
}
