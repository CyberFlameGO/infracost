package usage

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/infracost/infracost"
	"github.com/infracost/infracost/internal/schema"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v2"
	yamlv3 "gopkg.in/yaml.v3"
)

const minUsageFileVersion = "0.1"
const maxUsageFileVersion = "0.1"

type UsageFile struct { // nolint:revive
	Version       string                 `yaml:"version"`
	ResourceUsage map[string]interface{} `yaml:"resource_usage"`
}

type SyncResult struct {
	ResourceCount    int
	EstimationCount  int
	EstimationErrors map[string]error
}

func SyncUsageData(projects []*schema.Project, existingUsageData map[string]*schema.UsageData, usageFilePath string) (*SyncResult, error) {
	if usageFilePath == "" {
		return nil, nil
	}
	usageSchema, err := loadUsageSchema()
	if err != nil {
		return nil, err
	}

	// TODO: update this when we properly support multiple projects in usage
	resources := make([]*schema.Resource, 0)
	for _, project := range projects {
		resources = append(resources, project.Resources...)
	}

	syncResult, syncedResourcesUsage, hasUsage := syncResourcesUsage(resources, usageSchema, existingUsageData)

	docNode := &yamlv3.Node{
		Kind: yamlv3.DocumentNode,
	}
	rootNode := &yamlv3.Node{
		Kind: yamlv3.MappingNode,
	}
	docNode.Content = []*yamlv3.Node{rootNode}
	
	rootNode.Content = append(rootNode.Content, &yamlv3.Node{
		Kind: yamlv3.ScalarNode,
		Tag: "!!str",
		Value: "version",
	})
	rootNode.Content = append(rootNode.Content, &yamlv3.Node{
		Kind: yamlv3.ScalarNode,
		Tag: "!!str",
		Value: "0.1",
	})

	resourceUsageKeyNode := &yamlv3.Node{
		Kind: yamlv3.ScalarNode,
		Tag: "!!str",
		Value: "resource_usage",
	}
	if !hasUsage {
		resourceUsageKeyNode.Value = fmt.Sprintf("00__%s", resourceUsageKeyNode.Value)
	}

	rootNode.Content = append(rootNode.Content, resourceUsageKeyNode)
	rootNode.Content = append(rootNode.Content, syncedResourcesUsage)
	
	d, err := yamlv3.Marshal(docNode)
	d = bytes.ReplaceAll(d, []byte("00__"), []byte("# "))
	
	if err != nil {
		return nil, err
	}
	err = ioutil.WriteFile(usageFilePath, d, 0600)
	if err != nil {
		return nil, err
	}
	return &syncResult, nil
}

func syncResourcesUsage(resources []*schema.Resource, usageSchema map[string][]*schema.UsageSchemaItem, existingUsageData map[string]*schema.UsageData) (SyncResult, *yamlv3.Node, bool) {
	syncResult := SyncResult{EstimationErrors: make(map[string]error)}

	for _, resource := range resources {
		matchingUsageFileSchema, foundMatchingUsageFileSchema := findMatchingUsageFileSchema(usageSchema, resource)
		
		if resource.UsageSchema == nil && foundMatchingUsageFileSchema {
			// There is no explicitly defined UsageSchema for this resource. Use the old way and create one from
			// infracost-usage-example.yml.
			resource.UsageSchema = matchingUsageFileSchema
		}

		resourceUsage := make(map[string]interface{})
		for _, schemaItem := range resource.UsageSchema {
	
			var existingVal interface{}
			if existingUsage, ok := existingUsageData[resource.Name]; ok {
				switch schemaItem.ValueType {
				case schema.Float64:
					if v := existingUsage.GetFloat(schemaItem.Key); v != nil {
						existingVal = *v
					}
				case schema.Int64:
					if v := existingUsage.GetInt(schemaItem.Key); v != nil {
						existingVal = *v
					}
				case schema.String:
					if v := existingUsage.GetString(schemaItem.Key); v != nil {
						existingVal = *v
					}
				case schema.StringArray:
					if v := existingUsage.GetStringArray(schemaItem.Key); v != nil {
						existingVal = *v
					}
				}
			}
			if existingVal != nil {
				schemaItem.Value = existingVal
			}
			
			if schemaItem.Comment == "" && foundMatchingUsageFileSchema {
				var matchingItem *schema.UsageSchemaItem
				for _, item := range matchingUsageFileSchema {
					if item.Key == schemaItem.Key {
						matchingItem = item
						break
					}
				}
				if matchingItem != nil {
					schemaItem.Comment = matchingItem.Comment
				}
			}
		}

		syncResult.ResourceCount++
		if resource.EstimateUsage != nil {
			syncResult.EstimationCount++
			err := resource.EstimateUsage(context.TODO(), resourceUsage)
			if err != nil {
				syncResult.EstimationErrors[resource.Name] = err
				log.Warnf("Error estimating usage for resource %s: %v", resource.Name, err)
			}
		}
	}

	result, hasUsage := dumpResourceYAML(resources)
	return syncResult, result, hasUsage
}

func findMatchingUsageFileSchema(usageSchema map[string][]*schema.UsageSchemaItem, resource *schema.Resource) ([]*schema.UsageSchemaItem, bool) {
	addrParts := strings.Split(resource.Name, ".")
	if len(addrParts) < 2 {
		return []*schema.UsageSchemaItem{}, false
	}

	// This handles module names appearing in the resourceName too
	resourceTypeName := addrParts[len(addrParts)-2]
	matchingUsageFileSchema, ok := usageSchema[resourceTypeName]
	
	return matchingUsageFileSchema, ok
}

func dumpResourceYAML(resources []*schema.Resource) (*yamlv3.Node, bool) {
	rootNode := &yamlv3.Node{
		Kind: yamlv3.MappingNode,
	}

	hasUsage := false
	
	for _, resource := range resources {
		resourceKeyNode := &yamlv3.Node{
			Kind: yamlv3.ScalarNode,
			Tag: "!!str",
			Value: resource.Name,
		}
		
		resourceValNode := &yamlv3.Node{
			Kind: yamlv3.MappingNode,
		}
		
		resourceHasUsage := false
		
		for _, schemaItem := range resource.UsageSchema {
			kind := yamlv3.ScalarNode
			content := make([]*yamlv3.Node, 0)
		
			rawValue := schemaItem.Value
			if rawValue == nil {
				rawValue = schemaItem.DefaultValue
			}
			
			var tag string
			var value string
			
			switch schemaItem.ValueType {
			case schema.Float64:
				tag = "!!float"
				value = fmt.Sprintf("%f", rawValue)
			case schema.Int64:
				tag = "!!int"
				value = fmt.Sprintf("%d", rawValue)
			case schema.String:
				tag = "!!str"
				value = fmt.Sprintf("%s", rawValue)
			case schema.StringArray:
				tag = "!!seq"
				kind = yamlv3.SequenceNode
				for _, item := range rawValue.([]string) {
					content = append(content, &yamlv3.Node{
						Kind: yamlv3.ScalarNode,
						Tag: "!!str",
						Value: item,
					})
				}
			}
			
			itemKeyNode := &yamlv3.Node{
				Kind: yamlv3.ScalarNode,
				Tag: "!!str",
				Value: schemaItem.Key,
			}
			
			itemValNode := &yamlv3.Node{
				Kind: kind,
				Tag: tag,
				Value: value,
				Content: content,
				LineComment: schemaItem.Comment,
			}
			
			if schemaItem.Value == nil {
				itemKeyNode.Value = fmt.Sprintf("00__%s", itemKeyNode.Value)
			} else {
				resourceHasUsage = true
				hasUsage = true
			}
			
			resourceValNode.Content = append(resourceValNode.Content, itemKeyNode)
			resourceValNode.Content = append(resourceValNode.Content, itemValNode)
		}
			
		if !resourceHasUsage {
			resourceKeyNode.Value = fmt.Sprintf("00__%s", resourceKeyNode.Value)
		}

		rootNode.Content = append(rootNode.Content, resourceKeyNode)
		rootNode.Content = append(rootNode.Content, resourceValNode)
	}

	return rootNode, hasUsage
}

func commentize(s string) string {
	lines := make([]string, 0)

	split := strings.Split(s, "\n")

	for i, val := range split {
		if val == "" && i == len(split)-1 {
			lines = append(lines, val)
		} else {
			trimmed := strings.TrimSpace(val)
			indent := strings.Repeat(" ", len(val)-len(trimmed))
			lines = append(lines, fmt.Sprintf("%s# %s", indent, trimmed))
		}
	}
	return strings.Join(lines, "\n")
}


func loadUsageSchema() (map[string][]*schema.UsageSchemaItem, error) {
	usageSchema := make(map[string][]*schema.UsageSchemaItem)
	
	contents := infracost.GetReferenceUsageFileContents()
	
	rootNode := yamlv3.Node{}
	err := yamlv3.Unmarshal(*contents, &rootNode)
	if err != nil {
		return nil, errors.Wrap(err, "Error parsing usage YAML")
	}
	
	// TODO: check version
	
	// TODO: error checking
	resourceUsagesNode := rootNode.Content[0].Content[3]
	
	for i := 0; i < len(resourceUsagesNode.Content); i += 2 {
		keyNode := resourceUsagesNode.Content[i]
		valNode := resourceUsagesNode.Content[i+1]
	
		resourceType := strings.Split(keyNode.Value, ".")[0]
		usageSchema[resourceType] = make([]*schema.UsageSchemaItem, 0)
		for i := 0; i < len(valNode.Content); i += 2 {
			attrKeyNode := valNode.Content[i]
			attrValNode := valNode.Content[i+1]
	
			var defaultVal interface{}
			valType := schema.Int64
			defaultVal = 0
			
			if attrValNode.ShortTag() == "!!str" {
				valType = schema.String
				defaultVal = attrValNode.Value
			}
		
			usageSchema[resourceType] = append(usageSchema[resourceType], &schema.UsageSchemaItem{
				Key:          attrKeyNode.Value,
				ValueType:    valType,
				DefaultValue: defaultVal,
				Comment: attrValNode.LineComment,
			})
		}
	}
	
	return usageSchema, nil
}

func LoadFromFile(usageFilePath string, createIfNotExisting bool) (map[string]*schema.UsageData, error) {
	usageData := make(map[string]*schema.UsageData)

	if usageFilePath == "" {
		return usageData, nil
	}

	if createIfNotExisting {
		if _, err := os.Stat(usageFilePath); os.IsNotExist(err) {
			log.Debug("Specified usage file does not exist. It will be created")
			fileContent := yaml.MapSlice{
				{Key: "version", Value: "0.1"},
				{Key: "resource_usage", Value: make(map[string]interface{})},
			}
			d, err := yaml.Marshal(fileContent)
			if err != nil {
				return usageData, errors.Wrapf(err, "Error creating usage file")
			}
			err = ioutil.WriteFile(usageFilePath, d, 0600)
			if err != nil {
				return usageData, errors.Wrapf(err, "Error creating usage file")
			}
		}
	}

	log.Debug("Loading usage data from usage file")

	out, err := ioutil.ReadFile(usageFilePath)
	if err != nil {
		return usageData, errors.Wrapf(err, "Error reading usage file")
	}

	usageData, err = parseYAML(out)
	if err != nil {
		return usageData, errors.Wrapf(err, "Error parsing usage file")
	}

	return usageData, nil
}

func parseYAML(y []byte) (map[string]*schema.UsageData, error) {
	var usageFile UsageFile

	err := yaml.Unmarshal(y, &usageFile)
	if err != nil {
		return map[string]*schema.UsageData{}, errors.Wrap(err, "Error parsing usage YAML")
	}

	if !checkVersion(usageFile.Version) {
		return map[string]*schema.UsageData{}, fmt.Errorf("Invalid usage file version. Supported versions are %s ≤ x ≤ %s", minUsageFileVersion, maxUsageFileVersion)
	}

	usageMap := schema.NewUsageMap(usageFile.ResourceUsage)

	return usageMap, nil
}

func checkVersion(v string) bool {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return semver.Compare(v, "v"+minUsageFileVersion) >= 0 && semver.Compare(v, "v"+maxUsageFileVersion) <= 0
}
