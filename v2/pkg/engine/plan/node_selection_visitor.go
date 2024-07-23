package plan

import (
	"errors"
	"fmt"
	"slices"

	"github.com/wundergraph/graphql-go-tools/v2/pkg/ast"
	"github.com/wundergraph/graphql-go-tools/v2/pkg/astvisitor"
)

// nodeSelectionVisitor - walks through the operation multiple times to rewrite operation
// to be able to resolve fields from different datasources.
// During walks, it is adding required fields and rewrites abstract selection if it is necessary.
// We are revisiting query when we have:
// - new required fields were added to operation
// - when we have rewritten abstract field selection set
type nodeSelectionVisitor struct {
	debug DebugConfiguration

	operationName         string        // graphql query name
	operation, definition *ast.Document // graphql operation and schema documents
	walker                *astvisitor.Walker

	dataSources     []DataSource     // data sources configurations, which used by the current operation
	nodeSuggestions *NodeSuggestions // nodeSuggestions holds information about suggested data sources for each field

	selectionSetRefs []int // selectionSetRefs is a stack of selection set refs - used to add a required fields
	skipFieldsRefs   []int // skipFieldsRefs holds required field refs added by planner and should not be added to user response

	pendingRequiredFields       map[int]selectionSetPendingRequirementsNew // pendingRequiredFields is a map[selectionSetRef][]fieldsRequirementConfigNew
	visitedFieldsRequiresChecks map[string]struct{}                        // visitedFieldsRequiresChecks is a map[FieldRef] of already processed fields which we check for presence of @requires directive
	visitedFieldsKeyChecks      map[string]struct{}                        // visitedFieldsKeyChecks is a map[FieldRef] of already processed fields which we check for @key requirements
	visitedFieldsAbstractChecks map[int]struct{}                           // visitedFieldsAbstractChecks is a map[FieldRef] of already processed fields which we check for abstract type, e.g. union or interface
	fieldDependsOn              map[string][]int                           // fieldDependsOn is a map[fieldRef][]fieldRef - holds list of field refs which are required by a field ref, e.g. field should be planned only after required fields were planned
	fieldRequirementsConfigs    map[string][]FederationFieldConfiguration  // fieldRequirementsConfigs is a map[fieldRef]FederationFieldConfiguration - holds a list of required configuratuibs for a field ref to later built representation variables

	secondaryRun        bool // secondaryRun is a flag to indicate that we're running the nodeSelectionVisitor not the first time
	hasNewFields        bool // hasNewFields is used to determine if we need to run the planner again. It will be true in case required fields were added
	hasUnresolvedFields bool // hasUnresolvedFields is used to determine if we need to run the planner again. We should set it to true in case we have unresolved fields
}

func (c *nodeSelectionVisitor) shouldRevisit() bool {
	return c.hasNewFields || c.hasUnresolvedFields
}

// selectionSetPendingRequirements - is a wrapper to been able to have predictable order of fieldsRequirementConfigNew but at the same time deduplicate fieldsRequirementConfigNew
type selectionSetPendingRequirementsNew struct {
	existsTracker      map[string]struct{}          // existsTracker allows us to not add duplicated fieldsRequirementConfigNew
	requirementConfigs []fieldsRequirementConfigNew // requirementConfigs is a list of fieldsRequirementConfigNew which should be added to the selection set
}

// fieldsRequirementConfigNew is a mapping between requestedByPlannerID or requestedByFieldRef, which requested required fields,
// and selectionSet which should be added
type fieldsRequirementConfigNew struct {
	path         string
	selectionSet string
	skipTypename bool
	requestedBy  []requestedBy // requestedBy is a fieldRef + dsHash which requested fields via @requires or @key directive
}

type requestedBy struct {
	fieldRef int
	dsHash   DSHash
}

func (c *nodeSelectionVisitor) currentSelectionSet() int {
	if len(c.selectionSetRefs) == 0 {
		return ast.InvalidRef
	}

	return c.selectionSetRefs[len(c.selectionSetRefs)-1]
}

func (c *nodeSelectionVisitor) debugPrint(args ...any) {
	if !c.debug.ConfigurationVisitor {
		return
	}

	printArgs := []any{"[nodeSelectionVisitor]: "}
	printArgs = append(printArgs, args...)
	fmt.Println(printArgs...)
}

func (c *nodeSelectionVisitor) EnterDocument(operation, definition *ast.Document) {
	c.hasNewFields = false
	c.hasUnresolvedFields = false

	if c.selectionSetRefs == nil {
		c.selectionSetRefs = make([]int, 0, 8)
	} else {
		c.selectionSetRefs = c.selectionSetRefs[:0]
	}

	if c.secondaryRun {
		return
	}

	c.operation, c.definition = operation, definition

	if c.skipFieldsRefs == nil {
		c.skipFieldsRefs = make([]int, 0, 8)
	}

	c.visitedFieldsAbstractChecks = make(map[int]struct{})
	c.visitedFieldsRequiresChecks = make(map[string]struct{})
	c.visitedFieldsKeyChecks = make(map[string]struct{})
	c.pendingRequiredFields = make(map[int]selectionSetPendingRequirementsNew)

	c.fieldDependsOn = make(map[string][]int)
	c.fieldRequirementsConfigs = make(map[string][]FederationFieldConfiguration)
}

func (c *nodeSelectionVisitor) LeaveDocument(operation, definition *ast.Document) {

}

func (c *nodeSelectionVisitor) EnterOperationDefinition(ref int) {
	operationName := c.operation.OperationDefinitionNameString(ref)
	if c.operationName != operationName {
		c.walker.SkipNode()
		return
	}
}

func (c *nodeSelectionVisitor) EnterSelectionSet(ref int) {
	c.debugPrint("EnterSelectionSet ref:", ref)
	c.selectionSetRefs = append(c.selectionSetRefs, ref)
}

func (c *nodeSelectionVisitor) LeaveSelectionSet(ref int) {
	c.debugPrint("LeaveSelectionSet ref:", ref)
	c.processPendingRequiredFields(ref)
	c.selectionSetRefs = c.selectionSetRefs[:len(c.selectionSetRefs)-1]
}

func (c *nodeSelectionVisitor) EnterField(fieldRef int) {
	root := c.walker.Ancestors[0]
	if root.Kind != ast.NodeKindOperationDefinition {
		return
	}

	fieldName := c.operation.FieldNameUnsafeString(fieldRef)
	fieldAliasOrName := c.operation.FieldAliasOrNameString(fieldRef)
	typeName := c.walker.EnclosingTypeDefinition.NameString(c.definition)

	c.debugPrint("EnterField ref:", fieldRef, "fieldName:", fieldName, "typeName:", typeName)

	parentPath := c.walker.Path.DotDelimitedString()
	currentPath := parentPath + "." + fieldAliasOrName

	suggestions := c.nodeSuggestions.SuggestionsForPath(typeName, fieldName, currentPath)

	if currentPath == "query.me.details" {
		fmt.Println("REMOVE ME: "+
			"currentPath", "query.me.details")
	}

	for _, suggestion := range suggestions {
		// TODO: change SuggestionsForPath to return only selected suggestions
		if !suggestion.Selected {
			continue
		}

		dsIdx := slices.IndexFunc(c.dataSources, func(d DataSource) bool {
			return d.Hash() == suggestion.DataSourceHash
		})
		if dsIdx == -1 {
			c.walker.StopWithInternalErr(errors.New("we should always have a datasource for a suggestion"))
			return
		}
		ds := c.dataSources[dsIdx]

		// check if the field has @requires directive
		c.handleFieldRequiredByRequires(fieldRef, parentPath, typeName, fieldName, currentPath, ds)

		// check if a field type is abstract and need rewrites
		c.rewriteSelectionSetOfFieldWithInterfaceType(fieldRef, ds)

		// check if current field datasource is different from the parent node datasource
		c.handleFieldsRequiredByKey(fieldRef, parentPath, typeName, fieldName, currentPath, ds)
	}
}

func (c *nodeSelectionVisitor) LeaveField(ref int) {
}

func (c *nodeSelectionVisitor) handleFieldRequiredByRequires(fieldRef int, parentPath, typeName, fieldName, currentPath string, dsConfig DataSource) {
	fieldKey := fmt.Sprintf("%d.%d", fieldRef, dsConfig.Hash())
	_, visited := c.visitedFieldsRequiresChecks[fieldKey]
	if visited {
		return
	}
	c.visitedFieldsRequiresChecks[fieldKey] = struct{}{}

	if fieldName == typeNameField {
		// the __typename field could not have @requires directive
		return
	}

	requiresConfiguration, exists := dsConfig.RequiredFieldsByRequires(typeName, fieldName)
	if !exists {
		// we do not have a @requires configuration for the field
		return
	}

	// we should plan adding required fields for the field
	// they will be added in the on LeaveSelectionSet callback for the current selection set
	// and current field ref will be added to fieldDependsOn map
	c.planAddingRequiredFields(fieldRef, dsConfig.Hash(), requiresConfiguration, true, parentPath, currentPath)
	c.hasNewFields = true
}

func (c *nodeSelectionVisitor) handleFieldsRequiredByKey(fieldRef int, parentPath, typeName, fieldName, currentPath string, dsConfig DataSource) {
	fieldKey := fmt.Sprintf("%d.%d", fieldRef, dsConfig.Hash())
	_, visited := c.visitedFieldsKeyChecks[fieldKey]
	if visited {
		return
	}
	c.visitedFieldsKeyChecks[fieldKey] = struct{}{}

	_, hasRequiresCondition := dsConfig.RequiredFieldsByRequires(typeName, fieldName)

	treeNodeID := TreeNodeID(fieldRef)
	treeNode, ok := c.nodeSuggestions.responseTree.Find(treeNodeID)
	if !ok {
		return
	}

	// TODO: refactor
	parentSuggestions := treeNode.GetParent().GetData()
	var selectedParentsDSHashes []DSHash
	for _, itemID := range parentSuggestions {
		if c.nodeSuggestions.items[itemID].Selected {
			selectedParentsDSHashes = append(selectedParentsDSHashes, c.nodeSuggestions.items[itemID].DataSourceHash)
		}
	}

	// we should handle key requirements only when the datasource hash differs from the parent datasource hash
	// it means that this field should be resolved by another datasource
	// one exception in case field has requires directive - then field is planned on the same datasource
	// but fields with requires waits for the required fields to be resolved
	sameAsParentDS := len(selectedParentsDSHashes) == 1 && selectedParentsDSHashes[0] == dsConfig.Hash()

	if sameAsParentDS && !hasRequiresCondition {
		return
	}

	/*
		1. Same as parent datasource - the most simple case we just need to use the first available key configuration
		2. Different parent datasource - we need to check all parent datasources and do a match for the key configuration
		3. There is no matching key configuration, we should find a key configuration which is possible to plan

	*/

	requiredFieldsForType := dsConfig.RequiredFieldsByKey(typeName)

	if len(requiredFieldsForType) == 0 && hasRequiresCondition {
		// required fields could be of zero length in case type is not entity
		// or when entity has disabled entity resolver.
		// Usually we can't jump to the entity with disabled entity resolver, but there is one known exception
		// When entity has disabled entity resolver, but we have field with requires directive on this entity
		// we should add key fields for the field with requires - to pass them into field resolver

		keys := dsConfig.FederationConfiguration().Keys
		requiredFieldsForType = keys.FilterByTypeAndResolvability(typeName, false)
	}

	if len(requiredFieldsForType) == 0 && !sameAsParentDS {
		// TODO: planner error
		return
	}

	// 1. Current field datasource is the same as parent datasource, and field has requires directive defined
	if sameAsParentDS {
		c.planAddingRequiredFields(fieldRef, dsConfig.Hash(), requiredFieldsForType[0], false, parentPath, currentPath)
		c.hasNewFields = true
		return
	}

	isInterfaceObject := dsConfig.HasInterfaceObject(typeName)
	_ = isInterfaceObject

	// 2. check parent datasources for a matching key configuration
	if c.matchDataSourcesByKeyConfiguration(fieldRef, dsConfig.Hash(), parentPath, typeName, currentPath, requiredFieldsForType, isInterfaceObject, selectedParentsDSHashes) {
		c.hasNewFields = true
		return
	}

	// 3. check sibling datasources for a matching key configuration
	siblingIndexes := treeNodeSiblings(treeNode)
	siblingDS := make([]DSHash, 0, len(siblingIndexes))
	for _, siblingIndex := range siblingIndexes {
		if c.nodeSuggestions.items[siblingIndex].DataSourceHash == dsConfig.Hash() {
			continue
		}

		siblingDS = append(siblingDS, c.nodeSuggestions.items[siblingIndex].DataSourceHash)
	}

	if c.matchDataSourcesByKeyConfiguration(fieldRef, dsConfig.Hash(), parentPath, typeName, currentPath, requiredFieldsForType, isInterfaceObject, siblingDS) {
		c.hasNewFields = true
		return
	}

	// 4. check all datasources are they able to resolve the key configuration fields
}

func (c *nodeSelectionVisitor) matchDataSourcesByKeyConfiguration(fieldRef int, dsHash DSHash, parentPath, typeName, currentPath string, possibleRequiredFields []FederationFieldConfiguration, forInterfaceObject bool, dsHashes []DSHash) (matched bool) {
	for _, ds := range c.dataSources {
		if !slices.Contains(dsHashes, ds.Hash()) {
			continue
		}

		for _, possibleRequiredFieldConfig := range possibleRequiredFields {
			if ds.HasKeyRequirement(typeName, possibleRequiredFieldConfig.SelectionSet) {
				isInterfaceObject := ds.HasInterfaceObject(typeName)
				skipTypename := forInterfaceObject && isInterfaceObject

				c.planAddingRequiredFields(fieldRef, dsHash, possibleRequiredFieldConfig, skipTypename, parentPath, currentPath)

				return true
			}
		}
	}

	return false
}

func (c *nodeSelectionVisitor) planAddingRequiredFields(requestedByFieldRef int, dsHash DSHash, fieldConfiguration FederationFieldConfiguration, skipTypename bool, parentPath string, currentPath string) {
	currentSelectionSet := c.currentSelectionSet()

	requirements, hasRequirements := c.pendingRequiredFields[currentSelectionSet]

	if !hasRequirements {
		requirements = selectionSetPendingRequirementsNew{
			existsTracker: make(map[string]struct{}),
		}
	}

	existsKey := fmt.Sprintf("%d.%s.%s.%s", dsHash, parentPath, fieldConfiguration.SelectionSet, fieldConfiguration.FieldName)
	if _, exists := requirements.existsTracker[existsKey]; !exists {
		config := fieldsRequirementConfigNew{
			path:         currentPath,
			requestedBy:  []requestedBy{{fieldRef: requestedByFieldRef, dsHash: dsHash}},
			selectionSet: fieldConfiguration.SelectionSet,
			skipTypename: skipTypename,
		}

		requirements.existsTracker[existsKey] = struct{}{}
		requirements.requirementConfigs = append(requirements.requirementConfigs, config)
	}

	for i := range requirements.requirementConfigs {
		if requirements.requirementConfigs[i].selectionSet == fieldConfiguration.SelectionSet {
			if slices.IndexFunc(requirements.requirementConfigs[i].requestedBy, func(r requestedBy) bool {
				return r.fieldRef == requestedByFieldRef && r.dsHash == dsHash
			}) == -1 {
				requirements.requirementConfigs[i].requestedBy = append(requirements.requirementConfigs[i].requestedBy, requestedBy{fieldRef: requestedByFieldRef, dsHash: dsHash})
			}
			if !skipTypename {
				requirements.requirementConfigs[i].skipTypename = false
			}
			break
		}
	}

	c.pendingRequiredFields[currentSelectionSet] = requirements
	fieldKey := fmt.Sprintf("%d.%d", requestedByFieldRef, dsHash)
	c.fieldRequirementsConfigs[fieldKey] = append(c.fieldRequirementsConfigs[fieldKey], fieldConfiguration)
}

func (c *nodeSelectionVisitor) processPendingRequiredFields(selectionSetRef int) {
	configs, hasSelectionSet := c.pendingRequiredFields[selectionSetRef]
	if !hasSelectionSet {
		return
	}

	for _, requiredFieldsCfg := range configs.requirementConfigs {
		c.addRequiredFieldsToOperation(selectionSetRef, requiredFieldsCfg)
	}

	delete(c.pendingRequiredFields, selectionSetRef)
}

func (c *nodeSelectionVisitor) addRequiredFieldsToOperation(selectionSetRef int, requiredFieldsCfg fieldsRequirementConfigNew) {
	typeName := c.walker.EnclosingTypeDefinition.NameString(c.definition)
	key, report := RequiredFieldsFragment(typeName, requiredFieldsCfg.selectionSet, !requiredFieldsCfg.skipTypename)
	if report.HasErrors() {
		c.walker.StopWithInternalErr(fmt.Errorf("failed to parse required fields %s for %s at path %s", requiredFieldsCfg.selectionSet, typeName, requiredFieldsCfg.path))
		return
	}

	input := &addRequiredFieldsInput{
		key:                   key,
		operation:             c.operation,
		definition:            c.definition,
		report:                report,
		operationSelectionSet: selectionSetRef,
	}

	skipFieldRefs, requiredFieldRefs := addRequiredFields(input)
	if report.HasErrors() {
		c.walker.StopWithInternalErr(fmt.Errorf("failed to add required fields %s for %s at path %s", requiredFieldsCfg.selectionSet, typeName, requiredFieldsCfg.path))
		return
	}

	c.skipFieldsRefs = append(c.skipFieldsRefs, skipFieldRefs...)
	// add mapping for the field dependencies
	for _, requestedBy := range requiredFieldsCfg.requestedBy {
		fieldKey := fmt.Sprintf("%d.%d", requestedBy.fieldRef, requestedBy.dsHash)
		c.fieldDependsOn[fieldKey] = append(c.fieldDependsOn[fieldKey], requiredFieldRefs...)
	}
}

func (c *nodeSelectionVisitor) rewriteSelectionSetOfFieldWithInterfaceType(fieldRef int, ds DataSource) {
	if _, ok := c.visitedFieldsAbstractChecks[fieldRef]; ok {
		return
	}
	c.visitedFieldsAbstractChecks[fieldRef] = struct{}{}

	upstreamSchema, ok := ds.UpstreamSchema()
	if !ok {
		return
	}

	rewriter := newFieldSelectionRewriter(c.operation, c.definition)
	rewriter.SetUpstreamDefinition(upstreamSchema)
	rewriter.SetDatasourceConfiguration(ds)

	rewritten, err := rewriter.RewriteFieldSelection(fieldRef, c.walker.EnclosingTypeDefinition)

	if err != nil {
		c.walker.StopWithInternalErr(err)
		return
	}

	if !rewritten {
		return
	}

	c.hasNewFields = true
	c.walker.Stop()
}
