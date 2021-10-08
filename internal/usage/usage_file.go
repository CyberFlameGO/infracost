package usage

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
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

type ResourceUsage struct {
	Key   string
	Items map[string]*ResourceUsageItem
}

func (r *ResourceUsage) Map() map[string]interface{} {
	m := make(map[string]interface{}, len(r.Items))
	for k, item := range r.Items {
		m[k] = item.Value
	}

	return m
}

type ResourceUsageItem struct {
	*schema.UsageSchemaItem
	DefaultValue interface{}
	Value        interface{}
}

func SyncUsageData(projects []*schema.Project, existingUsageData map[string]*schema.UsageData, usageFilePath string) (*SyncResult, error) {
	if usageFilePath == "" {
		return nil, nil
	}
	referenceUsageSchema, err := loadReferenceUsageSchema()
	if err != nil {
		return nil, err
	}

	// TODO: update this when we properly support multiple projects in usage
	resources := make([]*schema.Resource, 0)
	for _, project := range projects {
		resources = append(resources, project.Resources...)
	}

	syncResult, syncedResourcesUsage := syncResourcesUsage(resources, referenceUsageSchema, existingUsageData)

	usageFile := UsageFile{
		Version: maxUsageFileVersion,
	}
	if syncedResourcesUsage != nil {
		usageFile.ResourceUsage = *syncedResourcesUsage
	}

	d, err := yamlv3.Marshal(usageFile)
	if err != nil {
		return nil, err
	}
	err = ioutil.WriteFile(usageFilePath, d, 0600)
	if err != nil {
		return nil, err
	}
	return &syncResult, nil
}

func syncResourcesUsage(resources []*schema.Resource, referenceUsageSchema map[string][]*schema.UsageSchemaItem, existingUsageData map[string]*schema.UsageData) (SyncResult, *yamlv3.Node) {
	syncResult := SyncResult{EstimationErrors: make(map[string]error)}

	resourcesUsages := make([]*ResourceUsage, 0, len(resources))

	for _, resource := range resources {
		resourceUsage := &ResourceUsage{
			Key: resource.Name,
		}

		// Sync with the old reference usage file
		matchingReferenceUsageSchema, ok := findMatchingReferenceUsageSchema(referenceUsageSchema, resource)
		if ok {
			syncResourceUsageWithSchema(resourceUsage, matchingReferenceUsageSchema)
		}

		// Sync with the new UsageSchema resource attribute
		if resource.UsageSchema != nil {
			syncResourceUsageWithSchema(resourceUsage, resource.UsageSchema)
		}

		// Sync the existing usage data from the usage file
		existingResourceUsage := existingUsageData[resource.Name]
		if existingResourceUsage == nil {
			syncResourceUsageWithExisting(resourceUsage, existingResourceUsage)
		}

		syncResult.ResourceCount++
		if resource.EstimateUsage != nil {
			syncResult.EstimationCount++

			resourceUsageMap := resourceUsage.Map()
			err := resource.EstimateUsage(context.TODO(), resourceUsageMap)
			if err != nil {
				syncResult.EstimationErrors[resource.Name] = err
				log.Warnf("Error estimating usage for resource %s: %v", resource.Name, err)
			}

			// Sync with the estimated usage data
			// First we have to convert the usage map back into a UsageData struc
			estimatedUsageData := schema.NewUsageData(resource.Name, schema.ParseAttributes(resourceUsageMap))
			syncResourceUsageWithExisting(resourceUsage, estimatedUsageData)
		}

		resourcesUsages = append(resourcesUsages, resourceUsage)
	}

	result := resourceUsagesToYAMLNode(resourcesUsages)
	return syncResult, result
}

func findMatchingReferenceUsageSchema(usageSchema map[string][]*schema.UsageSchemaItem, resource *schema.Resource) ([]*schema.UsageSchemaItem, bool) {
	addrParts := strings.Split(resource.Name, ".")
	if len(addrParts) < 2 {
		return []*schema.UsageSchemaItem{}, false
	}

	// This handles module names appearing in the resourceName too
	resourceTypeName := addrParts[len(addrParts)-2]
	matchingUsageFileSchema, ok := usageSchema[resourceTypeName]

	return matchingUsageFileSchema, ok
}

func syncResourceUsageWithSchema(resourceUsage *ResourceUsage, usageSchema []*schema.UsageSchemaItem) {
	if resourceUsage.Items == nil {
		resourceUsage.Items = make(map[string]*ResourceUsageItem)
	}

	for _, item := range usageSchema {
		resourceUsageItem := &ResourceUsageItem{
			UsageSchemaItem: item,
		}

		if item.ValueType == schema.Items {
			subResourceUsageItem := &ResourceUsage{
				Key: item.Key,
			}
			val := item.DefaultValue.([]*schema.UsageSchemaItem)
			syncResourceUsageWithSchema(subResourceUsageItem, val)

			resourceUsageItem.DefaultValue = subResourceUsageItem
		} else {
			resourceUsageItem.DefaultValue = item.DefaultValue
		}

		resourceUsage.Items[item.Key] = resourceUsageItem
	}
}

func syncResourceUsageWithExisting(resourceUsage *ResourceUsage, existing *schema.UsageData) {
	if existing == nil {
		return
	}

	for key, item := range resourceUsage.Items {
		var val interface{}

		switch item.ValueType {
		case schema.Int64:
			if v := existing.GetInt(key); v != nil {
				val = *v
			}
		case schema.Float64:
			if v := existing.GetFloat(key); v != nil {
				val = *v
			}
		case schema.StringArray:
			if v := existing.GetStringArray(key); v != nil {
				val = *v
			}
		case schema.Items:
			subResourceUsage := &ResourceUsage{}
			subExisting := schema.NewUsageData(key, existing.Get(key).Map())
			syncResourceUsageWithExisting(subResourceUsage, subExisting)
		}

		item.Value = val
	}
}

func resourceUsagesToYAMLNode(resourceUsages []*ResourceUsage) *yamlv3.Node {
	rootNode := &yamlv3.Node{
		Kind: yamlv3.MappingNode,
	}

	for _, resourceUsage := range resourceUsages {
		if len(resourceUsage.Items) == 0 {
			continue
		}

		resourceKeyNode := &yamlv3.Node{
			Kind:  yamlv3.ScalarNode,
			Tag:   "!!str",
			Value: resourceUsage.Key,
		}

		resourceValNode := &yamlv3.Node{
			Kind: yamlv3.MappingNode,
		}

		for _, item := range resourceUsage.Items {
			kind := yamlv3.ScalarNode
			content := make([]*yamlv3.Node, 0)

			rawValue := item.Value
			if rawValue == nil {
				rawValue = item.DefaultValue
			}

			if item.ValueType == schema.Items {
				subResourceUsage := rawValue.(*ResourceUsage)
				subResourceValNode := resourceUsagesToYAMLNode([]*ResourceUsage{subResourceUsage})
				resourceValNode.Content = append(resourceValNode.Content, subResourceValNode.Content...)
				continue
			}

			var tag string
			var value string

			switch item.ValueType {
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
						Kind:  yamlv3.ScalarNode,
						Tag:   "!!str",
						Value: item,
					})
				}
			case schema.Items:
				tag = "!!map"
				kind = yamlv3.MappingNode
			}

			itemKeyNode := &yamlv3.Node{
				Kind:  yamlv3.ScalarNode,
				Tag:   "!!str",
				Value: item.Key,
			}

			itemValNode := &yamlv3.Node{
				Kind:    kind,
				Tag:     tag,
				Value:   value,
				Content: content,
			}

			resourceValNode.Content = append(resourceValNode.Content, itemKeyNode)
			resourceValNode.Content = append(resourceValNode.Content, itemValNode)
		}

		rootNode.Content = append(rootNode.Content, resourceKeyNode)
		rootNode.Content = append(rootNode.Content, resourceValNode)
	}

	return rootNode
}

func loadReferenceUsageSchema() (map[string][]*schema.UsageSchemaItem, error) {
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
				{Key: "version", Value: maxUsageFileVersion},
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

	if y.ResourceUsage.Kind != 0 {
		err = y.ResourceUsage.Decode(&m)
		if err != nil {
			return usageData, errors.Wrap(err, "Error parsing usage YAML")
		}
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
