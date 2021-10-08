package usage

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/infracost/infracost/internal/schema"
)

const minUsageFileVersion = "0.1"
const maxUsageFileVersion = "0.1"

type UsageFile struct { // nolint:revive
	Version       string      `yaml:"version"`
	ResourceUsage yamlv3.Node `yaml:"resource_usage"`
}

type ResourceUsage struct {
	Key   string
	Items []*schema.UsageItem
}

func (r *ResourceUsage) Map() map[string]interface{} {
	m := make(map[string]interface{}, len(r.Items))
	for _, item := range r.Items {
		m[item.Key] = item.Value
	}

	return m
}

func syncResourcesUsage(resources []*schema.Resource, referenceUsageSchema map[string][]*schema.UsageItem, existingUsageData map[string][]*schema.UsageItem) (SyncResult, *yamlv3.Node) {
	syncResult := SyncResult{EstimationErrors: make(map[string]error)}

	resourcesUsages := make([]*ResourceUsage, 0, len(resources))

	for _, resource := range resources {
		resourceUsage := &ResourceUsage{
			Key: resource.Name,
		}
		
		matchingReferenceUsageSchema, ok := findMatchingReferenceUsageSchema(referenceUsageSchema, resource)
		if ok {
			mergeUsageItems(resourceUsage.Items, matchingReferenceUsageSchema)
		}
		
		mergeUsageItems(resourceUsage.Items, resource.UsageSchema)

		// Sync the existing usage data from the usage file
		existingResourceUsage := existingUsageData[resource.Name]
		if existingResourceUsage == nil {
			mergeUsageItems(resourceUsage.Items, existingResourceUsage)
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

func findMatchingReferenceUsageSchema(usageSchema map[string][]*schema.UsageItem, resource *schema.Resource) ([]*schema.UsageItem, bool) {
	addrParts := strings.Split(resource.Name, ".")
	if len(addrParts) < 2 {
		return []*schema.UsageItem{}, false
	}

	// This handles module names appearing in the resourceName too
	resourceTypeName := addrParts[len(addrParts)-2]
	matchingUsageFileSchema, ok := usageSchema[resourceTypeName]

	return matchingUsageFileSchema, ok
}

func mergeUsageItems(dest []*schema.UsageItem, src []*schema.UsageItem) {
	destItemMap := make(map[string]*schema.UsageItem, len(dest))
	for _, item := range dest {
		destItemMap[item.Key] = item
	}
	
	for _, srcItem := range src {
		destItem, ok := destItemMap[srcItem.Key]
		if !ok {
			destItem := &schema.UsageItem{Key: srcItem.Key}
			dest = append(dest, destItem)
		}
		
		destItem.ValueType = srcItem.ValueType
		destItem.Description = srcItem.Description
		
		if srcItem.ValueType == schema.Items {
			srcDefaultValue := srcItem.DefaultValue.([]*schema.UsageItem)
			srcValue := srcItem.Value.([]*schema.UsageItem)
			
			if destItem.DefaultValue == nil {
				destItem.DefaultValue = make([]*schema.UsageItem, 0)
			}
			if destItem.Value == nil {
				destItem.Value = make([]*schema.UsageItem, 0)
			}

			mergeUsageItems(destItem.DefaultValue.([]*schema.UsageItem), srcDefaultValue)			
			mergeUsageItems(destItem.Value.([]*schema.UsageItem), srcValue)
		} else {
			destItem.DefaultValue = srcItem.DefaultValue
			destItem.Value = srcItem.Value
		}
	}
}

func syncResourceUsageWithExisting(resourceUsage *ResourceUsage, existing *schema.UsageData) {
	if existing == nil {
		return
	}

	for _, item := range resourceUsage.Items {
		var val interface{}

		switch item.ValueType {
		case schema.Int64:
			if v := existing.GetInt(item.Key); v != nil {
				val = *v
			}
		case schema.Float64:
			if v := existing.GetFloat(item.Key); v != nil {
				val = *v
			}
		case schema.StringArray:
			if v := existing.GetStringArray(item.Key); v != nil {
				val = *v
			}
		case schema.Items:
			subResourceUsage := &ResourceUsage{}
			subExisting := schema.NewUsageData(item.Key, existing.Get(item.Key).Map())
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
				subResourceItems := rawValue.([]*schema.UsageItem)
				subResourceUsage := &ResourceUsage{
					Key:   item.Key,
					Items: subResourceItems,
				}
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
				Kind:        kind,
				Tag:         tag,
				Value:       value,
				Content:     content,
				LineComment: item.Description,
			}

			resourceValNode.Content = append(resourceValNode.Content, itemKeyNode)
			resourceValNode.Content = append(resourceValNode.Content, itemValNode)
		}

		rootNode.Content = append(rootNode.Content, resourceKeyNode)
		rootNode.Content = append(rootNode.Content, resourceValNode)
	}

	return rootNode
}
