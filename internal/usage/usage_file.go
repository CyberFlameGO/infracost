package usage

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v2"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/infracost/infracost"
	"github.com/infracost/infracost/internal/schema"
)

const minUsageFileVersion = "0.1"
const maxUsageFileVersion = "0.1"

type UsageFile struct { // nolint:revive
	Version       string      `yaml:"version"`
	ResourceUsage yamlv3.Node `yaml:"resource_usage"`
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

	syncResult, syncedResourcesUsage := syncResourcesUsage(resources, usageSchema, existingUsageData)
	// yaml.MapSlice is used to maintain the order of keys, so re-running
	// the code won't change the output.
	syncedUsageData := yaml.MapSlice{
		{Key: "version", Value: 0.1},
		{Key: "resource_usage", Value: syncedResourcesUsage},
	}
	d, err := yaml.Marshal(syncedUsageData)
	if err != nil {
		return nil, err
	}
	err = ioutil.WriteFile(usageFilePath, d, 0600)
	if err != nil {
		return nil, err
	}
	return &syncResult, nil
}

func syncResourcesUsage(resources []*schema.Resource, usageSchema map[string][]*schema.UsageSchemaItem, existingUsageData map[string]*schema.UsageData) (SyncResult, yaml.MapSlice) {
	syncResult := SyncResult{EstimationErrors: make(map[string]error)}
	syncedResourceUsage := make(map[string]interface{})
	for _, resource := range resources {
		resourceName := resource.Name
		resourceUSchema := resource.UsageSchema
		if resource.UsageSchema == nil {
			// There is no explicitly defined UsageSchema for this resource.  Use the old way and create one from
			// infracost-usage-example.yml.
			resourceTypeNames := strings.Split(resourceName, ".")
			if len(resourceTypeNames) < 2 {
				// It's a resource with no name
				continue
			}
			// This handles module names appearing in the resourceName too
			resourceTypeName := resourceTypeNames[len(resourceTypeNames)-2]
			schemaItems, ok := usageSchema[resourceTypeName]
			if !ok {
				continue
			}

			resourceUSchema = schemaItemsToUsageSchemaItems(schemaItems)
		}

		var resourceUsageData *schema.UsageData
		if v, ok := existingUsageData[resourceName]; ok {
			resourceUsageData = v
		}

		resourceUsage := buildResourceUsage(resourceUSchema, resourceUsageData)

		syncResult.ResourceCount++
		if resource.EstimateUsage != nil {
			syncResult.EstimationCount++
			err := resource.EstimateUsage(context.TODO(), resourceUsage)
			if err != nil {
				syncResult.EstimationErrors[resourceName] = err
				log.Warnf("Error estimating usage for resource %s: %v", resourceName, err)
			}
		}
		syncedResourceUsage[resourceName] = resourceUsage
	}
	// yaml.MapSlice is used to maintain the order of keys, so re-running
	// the code won't change the output.
	result := mapToSortedMapSlice(syncedResourceUsage)
	return syncResult, result
}

func schemaItemsToUsageSchemaItems(schemaItems []*schema.UsageSchemaItem) []*schema.UsageSchemaItem {
	resourceUSchema := make([]*schema.UsageSchemaItem, 0, len(schemaItems))

	for _, s := range schemaItems {
		resourceUSchema = append(resourceUSchema, &schema.UsageSchemaItem{
			Key:          s.Key,
			DefaultValue: s.DefaultValue,
			ValueType:    s.ValueType,
		})
	}

	return resourceUSchema
}

func buildResourceUsage(schemaItems []*schema.UsageSchemaItem, existingUsageData *schema.UsageData) map[string]interface{} {
	usage := make(map[string]interface{})

	for _, usageSchemaItem := range schemaItems {
		usageKey := usageSchemaItem.Key
		usageValueType := usageSchemaItem.ValueType

		var existingUsageValue interface{}
		if existingUsageData != nil {
			switch usageValueType {
			case schema.Float64:
				if v := existingUsageData.GetFloat(usageKey); v != nil {
					existingUsageValue = *v
				}
			case schema.Int64:
				if v := existingUsageData.GetInt(usageKey); v != nil {
					existingUsageValue = *v
				}
			case schema.String:
				if v := existingUsageData.GetString(usageKey); v != nil {
					existingUsageValue = *v
				}
			case schema.StringArray:
				if v := existingUsageData.GetStringArray(usageKey); v != nil {
					existingUsageValue = *v
				}
			case schema.Items:
				if v := existingUsageData.Get(usageKey).Map(); v != nil {
					subData := schema.NewUsageData(usageKey, v)
					existingUsageValue = buildResourceUsage(
						usageSchemaItem.DefaultValue.([]*schema.UsageSchemaItem),
						subData,
					)
				}
			}
		}

		usage[usageKey] = usageSchemaItem.DefaultValue

		if usageValueType == schema.Items {
			usage[usageKey] = itemsToMap(usageSchemaItem.DefaultValue.([]*schema.UsageSchemaItem))
		}

		if existingUsageValue != nil {
			usage[usageKey] = existingUsageValue
		}
	}

	return usage
}

func itemsToMap(items []*schema.UsageSchemaItem) map[string]interface{} {
	m := make(map[string]interface{}, len(items))
	for _, item := range items {
		if item.ValueType == schema.Items {
			m[item.Key] = itemsToMap(item.DefaultValue.([]*schema.UsageSchemaItem))
			continue
		}

		m[item.Key] = item.DefaultValue
	}

	return m
}

func loadUsageSchema() (map[string][]*schema.UsageSchemaItem, error) {
	usageSchema := make(map[string][]*schema.UsageSchemaItem)

	usageFile, err := loadReferenceFile()
	if err != nil {
		return usageSchema, err
	}

	if len(usageFile.ResourceUsage.Content)%2 != 0 {
		log.Errorf("YAML resource usage contents are not divisible by 2")
		return usageSchema, errors.New("Error loading reference usage file: unexpected YAML format")
	}

	for i := 0; i < len(usageFile.ResourceUsage.Content); i += 2 {
		resourceKeyNode := usageFile.ResourceUsage.Content[i]
		resourceValNode := usageFile.ResourceUsage.Content[i+1]

		if len(resourceValNode.Content)%2 != 0 {
			log.Errorf("YAML resource value contents are not divisible by 2")
			return usageSchema, errors.New("Error loading reference usage file: unexpected YAML format")
		}

		resourceType := strings.Split(resourceKeyNode.Value, ".")[0]
		usageSchema[resourceType] = make([]*schema.UsageSchemaItem, 0, len(resourceValNode.Content)/2)

		for i := 0; i < len(resourceValNode.Content); i += 2 {
			attrKeyNode := resourceValNode.Content[i]
			attrValNode := resourceValNode.Content[i+1]

			schemaItem, err := toSchemaItem(attrKeyNode, attrValNode)
			if err != nil {
				return usageSchema, errors.Wrap(err, "Error loading reference usage file")
			}

			usageSchema[resourceType] = append(usageSchema[resourceType], schemaItem)
		}
	}

	return usageSchema, nil
}

// toSchema item turns a YAML key node and a YAML value node into a *SchemaItem. This function supports recursion
// to allow for YAML map nodes to be parsed into nested sets of SchemaItem
//
// e.g. given:
//
//		keyNode: &yaml.Node{
//			Value: "testKey",
//		}
//
//		valNode: &yaml.Node{
//			Kind: yaml.MappingNode,
//			Content: []*yaml.Node{
//				&yaml.Node{Value: "prop1"},
//				&yaml.Node{Value: "test"},
//				&yaml.Node{Value: "prop2"},
//				&yaml.Node{Value: "test2"},
//				&yaml.Node{Value: "prop3"},
//				&yaml.Node{
//					Kind: yaml.MappingNode,
//					Content: []*yaml.Node{
//						&yaml.Node{Value: "nested1"},
//						&yaml.Node{Value: "test3"},
//					},
//				},
//			},
//		}
//
// toSchemaItem will return:
//
// 		SchemaItem{
//				Key:          "testKey",
//				DefaultValue: []*SchemaItem{
//					{
//						Key: "prop1",
//						DefaultValue: "test",
//					},
//					{
//						Key: "prop2",
//						DefaultValue: "test2",
//					},
//					{
//						Key: "prop3",
//						DefaultValue: []*SchemaItem{
//							{
//								Key: "nested1",
//								DefaultValue: "test3",
//							},
//						},
//					},
//				},
//			}
//
func toSchemaItem(keyNode *yamlv3.Node, valNode *yamlv3.Node) (*schema.UsageSchemaItem, error) {
	if keyNode == nil || valNode == nil {
		log.Errorf("YAML contains nil key or value node")
		return nil, errors.New("unexpected YAML format")
	}

	var defaultValue interface{}
	var usageValueType schema.UsageVariableType

	switch valNode.ShortTag() {
	case "!!int":
		usageValueType = schema.Int64
		defaultValue = 0

	case "!!float":
		usageValueType = schema.Float64
		defaultValue = 0.0

	case "!!map":
		usageValueType = schema.Items

		if len(valNode.Content)%2 != 0 {
			log.Errorf("YAML map node contents are not divisible by 2")
			return nil, errors.New("unexpected YAML format")
		}

		items := make([]*schema.UsageSchemaItem, 0, len(valNode.Content)/2)

		for i := 0; i < len(valNode.Content); i += 2 {
			mapKeyNode := valNode.Content[i]
			mapValNode := valNode.Content[i+1]

			mapSchemaItem, err := toSchemaItem(mapKeyNode, mapValNode)
			if err != nil {
				return nil, err
			}

			items = append(items, mapSchemaItem)
		}

		defaultValue = items

	default:
		usageValueType = schema.String
		defaultValue = valNode.Value
	}

	return &schema.UsageSchemaItem{
		Key:          keyNode.Value,
		ValueType:    usageValueType,
		DefaultValue: defaultValue,
	}, nil
}

func mapToSortedMapSlice(input map[string]interface{}) yaml.MapSlice {
	result := make(yaml.MapSlice, 0)
	// sort keys of the input to maintain same output for different runs.
	keys := make([]string, 0)
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// Iterate over sorted keys
	for _, k := range keys {
		v := input[k]
		if casted, ok := v.(map[string]interface{}); ok {
			result = append(result, yaml.MapItem{Key: k, Value: mapToSortedMapSlice(casted)})
		} else {
			result = append(result, yaml.MapItem{Key: k, Value: v})
		}
	}
	return result
}

func loadReferenceFile() (UsageFile, error) {
	contents := infracost.GetReferenceUsageFileContents()
	if contents == nil {
		return UsageFile{}, errors.New("Could not find reference usage file")
	}

	return parseYAML(*contents)
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

	y, err := parseYAML(out)
	if err != nil {
		return usageData, errors.Wrapf(err, "Error parsing usage file")
	}

	var m map[string]interface{}
	err = y.ResourceUsage.Decode(&m)
	if err != nil {
		return usageData, errors.Wrap(err, "Error parsing usage YAML")
	}

	usageData = schema.NewUsageMap(m)

	return usageData, nil
}

func parseYAML(y []byte) (UsageFile, error) {
	var usageFile UsageFile

	err := yamlv3.Unmarshal(y, &usageFile)
	if err != nil {
		return usageFile, errors.Wrap(err, "Error parsing usage YAML")
	}

	if !checkVersion(usageFile.Version) {
		return usageFile, fmt.Errorf("Invalid usage file version. Supported versions are %s ≤ x ≤ %s", minUsageFileVersion, maxUsageFileVersion)
	}

	return usageFile, nil
}

func checkVersion(v string) bool {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return semver.Compare(v, "v"+minUsageFileVersion) >= 0 && semver.Compare(v, "v"+maxUsageFileVersion) <= 0
}
