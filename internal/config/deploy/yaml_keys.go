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
	node = resolveYAMLAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if w.visits++; w.visits > maxYAMLKeyVisits {
			return errTooManyYAMLKeys
		}

		key, value := node.Content[i], resolveYAMLAlias(node.Content[i+1])
		if value == nil {
			continue
		}

		if key.Kind == yaml.ScalarNode && key.Value == "<<" && key.ShortTag() == "!!merge" {
			if err := w.addMerged(k, value); err != nil {
				return err
			}

			continue
		}

		if value.ShortTag() == "!!null" {
			continue
		}

		child, ok := k.children[key.Value]
		if !ok {
			child = &yamlKeys{}
			k.children[key.Value] = child
		}

		if value.Kind == yaml.MappingNode {
			if child.children == nil {
				child.children = map[string]*yamlKeys{}
			}

			if err := w.addMapping(child, value); err != nil {
				return err
			}
		}
	}

	return nil
}

// addMerged adds the keys of a merge key value, a mapping or a sequence of mappings, to k.
func (w *yamlKeyWalker) addMerged(k *yamlKeys, value *yaml.Node) error {
	if value.Kind != yaml.SequenceNode {
		return w.addMapping(k, value)
	}

	for _, item := range value.Content {
		if err := w.addMapping(k, item); err != nil {
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
