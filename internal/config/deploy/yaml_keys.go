package deploy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v4"
)

// maxYAMLKeyVisits bounds the mapping entries yamlKeysOf visits. Aliases are expanded, so a small document
// can otherwise refer to an exponential number of entries.
const maxYAMLKeyVisits = 100_000

var errTooManyYAMLKeys = errors.New("yaml document refers to too many keys")

// yamlKeys records which keys a YAML mapping sets.
type yamlKeys struct {
	// children holds the keys set in a mapping value. It is nil for any other value.
	children map[string]*yamlKeys
}

// yamlKeyWalker collects yamlKeys within a visit budget.
type yamlKeyWalker struct {
	visits int
}

// yamlKeysOf returns the keys the first YAML document in contents sets. Keys set to null count as unset,
// because decoding null leaves a field unchanged.
func yamlKeysOf(contents []byte) (*yamlKeys, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(contents, &doc); err != nil {
		return nil, fmt.Errorf("failed to decode yaml: %w", err)
	}

	keys := &yamlKeys{children: map[string]*yamlKeys{}}

	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		if err := new(yamlKeyWalker).addMapping(keys, doc.Content[0]); err != nil {
			return nil, err
		}
	}

	return keys, nil
}

// addMapping adds the keys set by node to k, if node is a mapping.
func (w *yamlKeyWalker) addMapping(k *yamlKeys, node *yaml.Node) error {
	values, err := w.mappingValues(node)
	if err != nil {
		return err
	}

	for name, value := range values {
		value = resolveYAMLAlias(value)
		if value == nil || value.ShortTag() == "!!null" {
			continue
		}

		child := &yamlKeys{}
		k.children[name] = child

		if value.Kind == yaml.MappingNode {
			child.children = map[string]*yamlKeys{}
			if err := w.addMapping(child, value); err != nil {
				return err
			}
		}
	}

	return nil
}

// mappingValues resolves YAML's shallow merge precedence before walking children.
// Null values still shadow merged keys, although they do not count as set.
func (w *yamlKeyWalker) mappingValues(node *yaml.Node) (map[string]*yaml.Node, error) {
	node = resolveYAMLAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, nil
	}

	values := make(map[string]*yaml.Node)

	var merges []*yaml.Node

	for i := 0; i+1 < len(node.Content); i += 2 {
		if w.visits++; w.visits > maxYAMLKeyVisits {
			return nil, errTooManyYAMLKeys
		}

		key, value := node.Content[i], node.Content[i+1]

		if key.Kind == yaml.ScalarNode && key.Value == "<<" && key.ShortTag() == "!!merge" {
			merges = append(merges, value)
			continue
		}

		values[key.Value] = value
	}

	for _, merge := range merges {
		if err := w.addMerged(values, merge); err != nil {
			return nil, err
		}
	}

	return values, nil
}

// addMerged fills missing keys only: explicit keys and earlier sequence entries win.
func (w *yamlKeyWalker) addMerged(values map[string]*yaml.Node, value *yaml.Node) error {
	value = resolveYAMLAlias(value)
	if value == nil {
		return nil
	}

	if value.Kind != yaml.SequenceNode {
		merged, err := w.mappingValues(value)
		if err != nil {
			return err
		}

		for name, node := range merged {
			if _, exists := values[name]; !exists {
				values[name] = node
			}
		}

		return nil
	}

	for _, item := range value.Content {
		item = resolveYAMLAlias(item)
		if item == nil || item.Kind != yaml.MappingNode {
			continue
		}

		if err := w.addMerged(values, item); err != nil {
			return err
		}
	}

	return nil
}

// resolveYAMLAlias returns the node an alias refers to, or node itself.
func resolveYAMLAlias(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode; depth++ {
		if depth > 64 {
			return nil
		}

		node = node.Alias
	}

	return node
}

// field returns the keys of the struct field f, and whether f is set. A nil k means the keys are
// unknown, and every field counts as set.
func (k *yamlKeys) field(f reflect.StructField) (*yamlKeys, bool) {
	if k == nil {
		return nil, true
	}

	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "" || name == "-" {
		return nil, false
	}

	child, ok := k.children[name]

	return child, ok
}

// forStruct returns the keys k sets in a struct. A scalar value of a struct, such as
// `destroy: true`, sets its enabled key.
func (k *yamlKeys) forStruct() *yamlKeys {
	if k == nil || k.children != nil {
		return k
	}

	return &yamlKeys{children: map[string]*yamlKeys{"enabled": {}}}
}

// size estimates the memory k holds, in bytes.
func (k *yamlKeys) size() int {
	if k == nil {
		return 0
	}

	size := 16

	for name, child := range k.children {
		size += 64 + len(name) + child.size()
	}

	return size
}
