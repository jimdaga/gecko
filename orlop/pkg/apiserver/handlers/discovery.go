package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/munnerz/goautoneg"

	"github.com/openshift-online/gecko/orlop/pkg/apiserver/constants"
	"github.com/openshift-online/gecko/orlop/pkg/apiserver/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	openapihandler "k8s.io/kube-openapi/pkg/handler"
	openapispec "k8s.io/kube-openapi/pkg/validation/spec"

	"sigs.k8s.io/yaml"
)

// ResourceProvider provides access to registered resources.
type ResourceProvider interface {
	Resources() []types.ResourceInfo
}

// DiscoveryHandler handles API discovery requests.
type DiscoveryHandler struct {
	resources       []types.ResourceInfo
	advertiseStatus bool // whether to advertise /status subresource in discovery
	openAPIV2Spec   *openapispec.Swagger
	openAPIV2Once   sync.Once
	v2JSONCache     []byte
	v2ProtoCache    []byte
}

// DiscoveryOptions configures discovery behavior.
type DiscoveryOptions struct {
	// AdvertiseStatus controls whether the /status subresource is advertised
	// in discovery responses. Defaults to true if not explicitly set to false.
	AdvertiseStatus *bool
}

// NewDiscoveryHandler creates a new discovery handler.
// If opts is nil or AdvertiseStatus is nil, status subresource is advertised by default.
func NewDiscoveryHandler(provider ResourceProvider, opts *DiscoveryOptions) *DiscoveryHandler {
	advertiseStatus := true
	if opts != nil && opts.AdvertiseStatus != nil {
		advertiseStatus = *opts.AdvertiseStatus
	}
	return &DiscoveryHandler{
		resources:       provider.Resources(),
		advertiseStatus: advertiseStatus,
	}
}

// APIGroupList handles GET /apis
// Returns the list of API groups available.
func (h *DiscoveryHandler) APIGroupList(w http.ResponseWriter, r *http.Request) {
	// Discovery: GET /apis
	groups := make(map[string]*metav1.APIGroup)

	// Collect unique groups and their versions
	for _, res := range h.resources {
		group := res.GVK.Group
		version := res.GVK.Version

		if _, exists := groups[group]; !exists {
			groups[group] = &metav1.APIGroup{
				TypeMeta: metav1.TypeMeta{
					Kind:       "APIGroup",
					APIVersion: constants.APIVersionV1,
				},
				Name:     group,
				Versions: []metav1.GroupVersionForDiscovery{},
			}
		}

		// Add version if not already present
		versionExists := false
		for _, v := range groups[group].Versions {
			if v.Version == version {
				versionExists = true
				break
			}
		}

		if !versionExists {
			groups[group].Versions = append(groups[group].Versions, metav1.GroupVersionForDiscovery{
				GroupVersion: group + "/" + version,
				Version:      version,
			})
		}

		// Set preferred version (first one)
		if len(groups[group].Versions) == 1 {
			groups[group].PreferredVersion = groups[group].Versions[0]
		}
	}

	// Convert map to list
	groupList := &metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIGroupList",
			APIVersion: constants.APIVersionV1,
		},
		Groups: make([]metav1.APIGroup, 0, len(groups)),
	}

	for _, group := range groups {
		groupList.Groups = append(groupList.Groups, *group)
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(groupList)
}

// APIGroup handles GET /apis/{group}
// Returns the list of versions for a specific API group.
func (h *DiscoveryHandler) APIGroup(w http.ResponseWriter, r *http.Request, group string) {
	// Discovery: GET /apis/{group}
	apiGroup := &metav1.APIGroup{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIGroup",
			APIVersion: constants.APIVersionV1,
		},
		Name:     group,
		Versions: []metav1.GroupVersionForDiscovery{},
	}

	// Collect versions for this group
	versions := make(map[string]bool)
	for _, res := range h.resources {
		if res.GVK.Group == group {
			version := res.GVK.Version
			if !versions[version] {
				versions[version] = true
				apiGroup.Versions = append(apiGroup.Versions, metav1.GroupVersionForDiscovery{
					GroupVersion: group + "/" + version,
					Version:      version,
				})
			}
		}
	}

	if len(apiGroup.Versions) == 0 {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}

	// Set preferred version (first one)
	apiGroup.PreferredVersion = apiGroup.Versions[0]

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(apiGroup)
}

// APIResourceList handles GET /apis/{group}/{version}
// Returns the list of resources for a specific API group version.
func (h *DiscoveryHandler) APIResourceList(w http.ResponseWriter, r *http.Request, group, version string) {
	// Discovery: GET /apis/{group}/{version}
	resourceList := &metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIResourceList",
			APIVersion: constants.APIVersionV1,
		},
		GroupVersion: group + "/" + version,
		APIResources: []metav1.APIResource{},
	}

	// Find resources for this group/version
	for _, res := range h.resources {
		if res.GVK.Group == group && res.GVK.Version == version {
			// Build the advertised verb list. When +orlop:public-verbs is set on the
			// resource's package, only those verbs are advertised; otherwise all
			// standard verbs are included for full backward compatibility.
			allVerbs := metav1.Verbs{"create", "delete", "get", "list", "patch", "update", "watch"}
			advertiseVerbs := allVerbs
			if len(res.Verbs) > 0 {
				advertiseVerbs = metav1.Verbs(res.Verbs)
			}
			resource := metav1.APIResource{
				Name:         res.Plural,
				SingularName: res.Singular,
				Kind:         res.GVK.Kind,
				Namespaced:   res.Namespaced,
				Verbs:        advertiseVerbs,
			}

			// Add main resource
			resourceList.APIResources = append(resourceList.APIResources, resource)

			// Add status subresource only if advertised
			if h.advertiseStatus {
				statusResource := metav1.APIResource{
					Name:         res.Plural + "/status",
					SingularName: res.Singular,
					Kind:         res.GVK.Kind,
					Namespaced:   res.Namespaced,
					Verbs:        metav1.Verbs{"get", "patch", "update"},
				}
				resourceList.APIResources = append(resourceList.APIResources, statusResource)
			}
		}
	}

	if len(resourceList.APIResources) == 0 {
		writeError(w, http.StatusNotFound, "group version not found")
		return
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resourceList)
}

// OpenAPIV3 handles GET /openapi/v3
// Returns the list of available OpenAPI v3 group versions.
func (h *DiscoveryHandler) OpenAPIV3(w http.ResponseWriter, r *http.Request) {
	// Discovery: GET /openapi/v3
	// Build a list of group versions
	groupVersions := make(map[string]bool)
	for _, res := range h.resources {
		gv := res.GVK.Group + "/" + res.GVK.Version
		groupVersions[gv] = true
	}

	// Convert to OpenAPI v3 discovery format
	paths := make(map[string]interface{})
	for gv := range groupVersions {
		paths["apis/"+gv] = map[string]interface{}{
			"serverRelativeURL": "/openapi/v3/apis/" + gv,
		}
	}

	response := map[string]interface{}{
		"paths": paths,
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

// --- OpenAPI response helpers ---

// v3Response builds an OpenAPI v3 response object with an optional JSON schema reference.
func v3Response(description, schemaRef string) map[string]interface{} {
	resp := map[string]interface{}{"description": description}
	if schemaRef != "" {
		resp["content"] = map[string]interface{}{
			constants.ContentTypeJSON: map[string]interface{}{
				"schema": map[string]interface{}{"$ref": schemaRef},
			},
		}
	}
	return resp
}

// v3Operation builds an OpenAPI v3 operation with standard response.
func v3Operation(description, statusCode, statusDesc, schemaRef string) map[string]interface{} {
	op := map[string]interface{}{
		"description": description,
		"responses": map[string]interface{}{
			statusCode: v3Response(statusDesc, schemaRef),
		},
	}
	return op
}

// v3OperationWithID builds an OpenAPI v3 operation with an operationId.
func v3OperationWithID(operationId, description, statusCode, statusDesc, schemaRef string) map[string]interface{} {
	op := v3Operation(description, statusCode, statusDesc, schemaRef)
	op["operationId"] = operationId
	return op
}

// httpMethodToVerb maps HTTP methods used in OpenAPI paths to the corresponding
// Kubernetes verb used in +orlop:public-verbs. "post" maps to "create",
// "get" maps to both "get" and "list" depending on context — callers should
// pass the appropriate verb directly when building collection vs. item paths.
// This mapping is used to filter path entries by VerbAllowed.
var httpMethodToVerb = map[string]string{
	"post":   "create",
	"put":    "update",
	"patch":  "patch",
	"delete": "delete",
}

// filterPathEntry removes HTTP method keys from a path entry map when the
// corresponding verb is not permitted by res.VerbAllowed. The "get" key is
// checked against the supplied listOrGet verb ("list" for collection paths,
// "get" for item paths). "parameters" is always kept.
func filterPathEntry(entry map[string]interface{}, res types.ResourceInfo, listOrGet string) map[string]interface{} {
	if len(res.Verbs) == 0 {
		return entry
	}
	filtered := make(map[string]interface{})
	for k, v := range entry {
		if k == "parameters" {
			filtered[k] = v
			continue
		}
		verb, ok := httpMethodToVerb[k]
		if !ok {
			// "get" — resolve to list or get depending on caller-supplied context
			if k == "get" {
				verb = listOrGet
			} else {
				// Unknown key, keep it
				filtered[k] = v
				continue
			}
		}
		if res.VerbAllowed(verb) {
			filtered[k] = v
		}
	}
	return filtered
}

// v3ResourcePaths generates OpenAPI v3 collection, item, and status paths for a resource.
// Verb entries for verbs not permitted by res.VerbAllowed are omitted.
func v3ResourcePaths(basePath string, params []interface{}, nameParam map[string]interface{}, kind, plural, schemaRef string, res types.ResourceInfo) map[string]map[string]interface{} {
	paths := make(map[string]map[string]interface{})

	// Collection: list + create
	collection := map[string]interface{}{
		"parameters": params,
		"get":        v3Operation("list "+plural, "200", "OK", schemaRef),
		"post":       v3Operation("create a "+kind, "201", "Created", schemaRef),
	}
	paths[basePath] = filterPathEntry(collection, res, "list")

	// Item: get + put + delete
	itemPath := basePath + "/{name}"
	itemParams := append(append([]interface{}{}, params...), nameParam)
	item := map[string]interface{}{
		"parameters": itemParams,
		"get":        v3Operation("read the specified "+kind, "200", "OK", schemaRef),
		"put":        v3Operation("replace the specified "+kind, "200", "OK", schemaRef),
		"delete":     v3Operation("delete a "+kind, "200", "OK", ""),
	}
	paths[itemPath] = filterPathEntry(item, res, "get")

	// Status subresource: get + put (not filtered by public-verbs — status is
	// a separate concern; advertiseStatus already gates whether this appears)
	statusPath := itemPath + "/status"
	paths[statusPath] = map[string]interface{}{
		"parameters": itemParams,
		"get":        v3Operation("read status of the specified "+kind, "200", "OK", schemaRef),
		"put":        v3Operation("replace status of the specified "+kind, "200", "OK", schemaRef),
	}

	return paths
}

// v3NestedResourcePaths generates OpenAPI v3 paths for nested (parent/child) resources.
func v3NestedResourcePaths(nestedBase string, params []interface{}, nameParam map[string]interface{}, kind, plural, parentPlural, schemaRef string) map[string]map[string]interface{} {
	paths := make(map[string]map[string]interface{})

	// Nested collection
	paths[nestedBase] = map[string]interface{}{
		"parameters": params,
		"get":        v3OperationWithID("listNamespaced"+kind+"For"+parentPlural, "list "+plural+" for a specific parent", "200", "OK", schemaRef),
		"post":       v3OperationWithID("createNamespaced"+kind+"For"+parentPlural, "create a "+kind+" under a specific parent", "201", "Created", schemaRef),
	}

	// Nested item
	nestedItem := nestedBase + "/{name}"
	itemParams := append(append([]interface{}{}, params...), nameParam)
	paths[nestedItem] = map[string]interface{}{
		"parameters": itemParams,
		"get":        v3OperationWithID("readNamespaced"+kind+"For"+parentPlural, "read the specified "+kind+" under a specific parent", "200", "OK", schemaRef),
		"put":        v3OperationWithID("replaceNamespaced"+kind+"For"+parentPlural, "replace the specified "+kind+" under a specific parent", "200", "OK", schemaRef),
		"delete":     v3OperationWithID("deleteNamespaced"+kind+"For"+parentPlural, "delete a "+kind+" under a specific parent", "200", "OK", ""),
	}

	// Nested status
	nestedStatus := nestedItem + "/status"
	paths[nestedStatus] = map[string]interface{}{
		"parameters": itemParams,
		"get":        v3OperationWithID("readNamespaced"+kind+"StatusFor"+parentPlural, "read status of the specified "+kind+" under a specific parent", "200", "OK", schemaRef),
		"put":        v3OperationWithID("replaceNamespaced"+kind+"StatusFor"+parentPlural, "replace status of the specified "+kind+" under a specific parent", "200", "OK", schemaRef),
	}

	return paths
}

// OpenAPIV3GroupVersion handles GET /openapi/v3/apis/{group}/{version}
// Returns the OpenAPI v3 schema for a specific group version.
func (h *DiscoveryHandler) OpenAPIV3GroupVersion(w http.ResponseWriter, r *http.Request, group, version string) {
	gv := runtimeschema.GroupVersion{Group: group, Version: version}

	spec := map[string]interface{}{
		"openapi": "3.0.0",
		"info": map[string]interface{}{
			"title":   group + "/" + version,
			"version": version,
		},
		"paths":      map[string]interface{}{},
		"components": map[string]interface{}{},
	}

	schemas := make(map[string]interface{})

	for _, res := range h.resources {
		if res.GVK.Group != group || res.GVK.Version != version {
			continue
		}

		var schemaObj map[string]interface{}
		if err := yaml.Unmarshal([]byte(res.SchemaYAML), &schemaObj); err != nil {
			continue
		}

		schemaObj["x-kubernetes-group-version-kind"] = []map[string]interface{}{
			{"group": res.GVK.Group, "version": res.GVK.Version, "kind": res.GVK.Kind},
		}

		schemaName := gv.String() + "." + res.GVK.Kind
		schemas[schemaName] = schemaObj

		paths := spec["paths"].(map[string]interface{})
		schemaRef := "#/components/schemas/" + schemaName

		namespaceParam := map[string]interface{}{
			"name": "namespace", "in": "path", "required": true,
			"schema": map[string]interface{}{"type": "string"}, "description": "object namespace",
		}
		nameParam := map[string]interface{}{
			"name": "name", "in": "path", "required": true,
			"schema": map[string]interface{}{"type": "string"}, "description": "name of the " + res.GVK.Kind,
		}

		basePath := "/apis/" + group + "/" + version + "/" + res.Plural
		params := []interface{}{}
		if res.Namespaced {
			basePath = "/apis/" + group + "/" + version + "/namespaces/{namespace}/" + res.Plural
			params = append(params, namespaceParam)
		}
		for p, entry := range v3ResourcePaths(basePath, params, nameParam, res.GVK.Kind, res.Plural, schemaRef, res) {
			paths[p] = entry
		}

		if res.ParentResource != nil {
			parentIDParam := map[string]interface{}{
				"name": "parentID", "in": "path", "required": true,
				"schema": map[string]interface{}{"type": "string"}, "description": "ID of the parent " + res.ParentResource.Plural,
			}
			nestedBase := "/apis/" + group + "/" + version + "/namespaces/{namespace}/" + res.ParentResource.Plural + "/{parentID}/" + res.Plural
			for p, entry := range v3NestedResourcePaths(nestedBase, []interface{}{namespaceParam, parentIDParam}, nameParam, res.GVK.Kind, res.Plural, res.ParentResource.Plural, schemaRef) {
				paths[p] = entry
			}
		}

		// Nested routes for child resources with a parent
		if res.ParentResource != nil {
			parentIDParam := map[string]interface{}{
				"name":        "parentID",
				"in":          "path",
				"required":    true,
				"schema":      map[string]interface{}{"type": "string"},
				"description": "ID of the parent " + res.ParentResource.Plural,
			}
			nestedBase := "/apis/" + group + "/" + version + "/namespaces/{namespace}/" + res.ParentResource.Plural + "/{parentID}/" + res.Plural

			paths[nestedBase] = map[string]interface{}{
				"parameters": []interface{}{namespaceParam, parentIDParam},
				"get": map[string]interface{}{
					"operationId": "listNamespaced" + res.GVK.Kind + "For" + res.ParentResource.Plural,
					"description": "list " + res.Plural + " for a specific parent",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": map[string]interface{}{
								constants.ContentTypeJSON: map[string]interface{}{
									"schema": map[string]interface{}{"$ref": schemaRef},
								},
							},
						},
					},
				},
				"post": map[string]interface{}{
					"operationId": "createNamespaced" + res.GVK.Kind + "For" + res.ParentResource.Plural,
					"description": "create a " + res.GVK.Kind + " under a specific parent",
					"responses": map[string]interface{}{
						"201": map[string]interface{}{
							"description": "Created",
							"content": map[string]interface{}{
								constants.ContentTypeJSON: map[string]interface{}{
									"schema": map[string]interface{}{"$ref": schemaRef},
								},
							},
						},
					},
				},
			}

			nestedItem := nestedBase + "/{name}"
			paths[nestedItem] = map[string]interface{}{
				"parameters": []interface{}{namespaceParam, parentIDParam, nameParam},
				"get": map[string]interface{}{
					"operationId": "readNamespaced" + res.GVK.Kind + "For" + res.ParentResource.Plural,
					"description": "read the specified " + res.GVK.Kind + " under a specific parent",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": map[string]interface{}{
								constants.ContentTypeJSON: map[string]interface{}{
									"schema": map[string]interface{}{"$ref": schemaRef},
								},
							},
						},
					},
				},
				"put": map[string]interface{}{
					"operationId": "replaceNamespaced" + res.GVK.Kind + "For" + res.ParentResource.Plural,
					"description": "replace the specified " + res.GVK.Kind + " under a specific parent",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": map[string]interface{}{
								constants.ContentTypeJSON: map[string]interface{}{
									"schema": map[string]interface{}{"$ref": schemaRef},
								},
							},
						},
					},
				},
				"delete": map[string]interface{}{
					"operationId": "deleteNamespaced" + res.GVK.Kind + "For" + res.ParentResource.Plural,
					"description": "delete a " + res.GVK.Kind + " under a specific parent",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "OK"},
					},
				},
			}

			nestedStatus := nestedItem + "/status"
			paths[nestedStatus] = map[string]interface{}{
				"parameters": []interface{}{namespaceParam, parentIDParam, nameParam},
				"get": map[string]interface{}{
					"operationId": "readNamespaced" + res.GVK.Kind + "StatusFor" + res.ParentResource.Plural,
					"description": "read status of the specified " + res.GVK.Kind + " under a specific parent",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": map[string]interface{}{
								constants.ContentTypeJSON: map[string]interface{}{
									"schema": map[string]interface{}{"$ref": schemaRef},
								},
							},
						},
					},
				},
				"put": map[string]interface{}{
					"operationId": "replaceNamespaced" + res.GVK.Kind + "StatusFor" + res.ParentResource.Plural,
					"description": "replace status of the specified " + res.GVK.Kind + " under a specific parent",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{
							"description": "OK",
							"content": map[string]interface{}{
								constants.ContentTypeJSON: map[string]interface{}{
									"schema": map[string]interface{}{"$ref": schemaRef},
								},
							},
						},
					},
				},
			}
		}
	}

	if len(schemas) == 0 {
		writeError(w, http.StatusNotFound, "group version not found")
		return
	}

	spec["components"] = map[string]interface{}{
		"schemas": schemas,
	}

	w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(spec)
}

// --- OpenAPI v2 response helpers ---

// v2Response builds an OpenAPI v2 response object with an optional schema reference.
func v2Response(description, defRef string) map[string]interface{} {
	resp := map[string]interface{}{"description": description}
	if defRef != "" {
		resp["schema"] = map[string]interface{}{"$ref": defRef}
	}
	return resp
}

// v2Operation builds an OpenAPI v2 operation.
func v2Operation(operationId, description string, produces, consumes []string, params []interface{}, statusCode, statusDesc, defRef string) map[string]interface{} {
	op := map[string]interface{}{
		"description": description,
		"operationId": operationId,
		"produces":    produces,
		"parameters":  params,
		"responses": map[string]interface{}{
			statusCode: v2Response(statusDesc, defRef),
		},
	}
	if len(consumes) > 0 {
		op["consumes"] = consumes
	}
	return op
}

// v2NestedResourcePaths generates OpenAPI v2 paths for nested (parent/child) resources.
func v2NestedResourcePaths(nestedBase string, nsParam, parentIDParam, nameParam map[string]interface{}, kind, plural, parentPlural, defRef string) map[string]map[string]interface{} {
	paths := make(map[string]map[string]interface{})
	jsonMime := []string{constants.ContentTypeJSON}
	ns := []interface{}{nsParam}

	// Collection
	paths[nestedBase] = map[string]interface{}{
		"get":  v2Operation(fmt.Sprintf("listNamespaced%sFor%s", kind, parentPlural), fmt.Sprintf("list %s for a specific parent", plural), jsonMime, nil, append(ns, parentIDParam), "200", "OK", defRef),
		"post": v2Operation(fmt.Sprintf("createNamespaced%sFor%s", kind, parentPlural), fmt.Sprintf("create a %s under a specific parent", kind), jsonMime, jsonMime, append(ns, parentIDParam, map[string]interface{}{"name": "body", "in": "body", "required": true, "schema": map[string]interface{}{"$ref": defRef}}), "201", "Created", defRef),
	}

	// Item
	nestedItem := nestedBase + "/{name}"
	itemParams := []interface{}{nsParam, parentIDParam, nameParam}
	paths[nestedItem] = map[string]interface{}{
		"get":    v2Operation(fmt.Sprintf("readNamespaced%sFor%s", kind, parentPlural), fmt.Sprintf("read the specified %s under a specific parent", kind), jsonMime, nil, itemParams, "200", "OK", defRef),
		"put":    v2Operation(fmt.Sprintf("replaceNamespaced%sFor%s", kind, parentPlural), fmt.Sprintf("replace the specified %s under a specific parent", kind), jsonMime, jsonMime, append(itemParams, map[string]interface{}{"name": "body", "in": "body", "required": true, "schema": map[string]interface{}{"$ref": defRef}}), "200", "OK", defRef),
		"delete": v2Operation(fmt.Sprintf("deleteNamespaced%sFor%s", kind, parentPlural), fmt.Sprintf("delete a %s under a specific parent", kind), jsonMime, nil, itemParams, "200", "OK", ""),
	}

	// Status
	nestedStatus := nestedItem + "/status"
	paths[nestedStatus] = map[string]interface{}{
		"get": v2Operation(fmt.Sprintf("readNamespaced%sStatusFor%s", kind, parentPlural), fmt.Sprintf("read status of the specified %s under a specific parent", kind), jsonMime, nil, itemParams, "200", "OK", defRef),
		"put": v2Operation(fmt.Sprintf("replaceNamespaced%sStatusFor%s", kind, parentPlural), fmt.Sprintf("replace status of the specified %s under a specific parent", kind), jsonMime, jsonMime, append(itemParams, map[string]interface{}{"name": "body", "in": "body", "required": true, "schema": map[string]interface{}{"$ref": defRef}}), "200", "OK", defRef),
	}

	return paths
}

// buildOpenAPIV2Spec builds the OpenAPI v2 spec as a openapispec.Swagger object.
func (h *DiscoveryHandler) buildOpenAPIV2Spec() *openapispec.Swagger {
	spec := map[string]interface{}{
		"swagger": "2.0",
		"info": map[string]interface{}{
			"title":   "Orlop API",
			"version": "v1",
		},
		"paths":       map[string]interface{}{},
		"definitions": map[string]interface{}{},
	}

	definitions := spec["definitions"].(map[string]interface{})
	paths := spec["paths"].(map[string]interface{})
	jsonMime := []string{constants.ContentTypeJSON}

	groupedResources := make(map[string][]types.ResourceInfo)
	for _, res := range h.resources {
		key := res.GVK.Group + "/" + res.GVK.Version
		groupedResources[key] = append(groupedResources[key], res)
	}

	for gv, resources := range groupedResources {
		for _, res := range resources {
			var schemaObj map[string]interface{}
			if err := yaml.Unmarshal([]byte(res.SchemaYAML), &schemaObj); err != nil {
				continue
			}

			schemaObj["x-kubernetes-group-version-kind"] = []map[string]interface{}{
				{"group": res.GVK.Group, "version": res.GVK.Version, "kind": res.GVK.Kind},
			}

			defName := res.GVK.Group + "." + res.GVK.Version + "." + res.GVK.Kind
			definitions[defName] = schemaObj
			defRef := fmt.Sprintf("#/definitions/%s", defName)
			basePath := fmt.Sprintf("/apis/%s/%s", gv, res.Plural)
			ver := res.GVK.Version
			kind := res.GVK.Kind

			nsParam := map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string", "description": "object name and auth scope, such as for teams and projects"}
			nameParam := map[string]interface{}{"name": "name", "in": "path", "required": true, "type": "string", "description": "name of the resource"}
			bodyParam := map[string]interface{}{"name": "body", "in": "body", "required": true, "schema": map[string]interface{}{"$ref": defRef}}
			collectionParams := []interface{}{}
			itemParams := []interface{}{nameParam}
			if res.Namespaced {
				basePath = fmt.Sprintf("/apis/%s/namespaces/{namespace}/%s", gv, res.Plural)
				collectionParams = append(collectionParams, nsParam)
				itemParams = append([]interface{}{nsParam}, itemParams...)
			}

			// Collection operations
			listParams := append([]interface{}{}, collectionParams...)
			listParams = append(listParams,
				map[string]interface{}{"name": "labelSelector", "in": "query", "type": "string", "description": "A selector to restrict the list of returned objects by their labels"},
				map[string]interface{}{"name": "watch", "in": "query", "type": "boolean", "description": "Watch for changes to the described resources"},
				map[string]interface{}{"name": "resourceVersion", "in": "query", "type": "string", "description": "When specified with watch, shows changes that occur after that version"},
			)
			createParams := append([]interface{}{}, collectionParams...)
			createParams = append(createParams, bodyParam)
			collectionEntry := map[string]interface{}{
				"get":  v2Operation(fmt.Sprintf("list%s%s", ver, kind), fmt.Sprintf("list objects of kind %s", kind), jsonMime, nil, listParams, "200", "OK", defRef),
				"post": v2Operation(fmt.Sprintf("create%s%s", ver, kind), fmt.Sprintf("create a %s", kind), jsonMime, jsonMime, createParams, "201", "Created", defRef),
			}
			paths[basePath] = filterPathEntry(collectionEntry, res, "list")

			// Item operations
			itemPath := basePath + "/{name}"
			itemEntry := map[string]interface{}{
				"get":    v2Operation(fmt.Sprintf("read%s%s", ver, kind), fmt.Sprintf("read the specified %s", kind), jsonMime, nil, itemParams, "200", "OK", defRef),
				"put":    v2Operation(fmt.Sprintf("replace%s%s", ver, kind), fmt.Sprintf("replace the specified %s", kind), jsonMime, jsonMime, append(append([]interface{}{}, itemParams...), bodyParam), "200", "OK", defRef),
				"delete": v2Operation(fmt.Sprintf("delete%s%s", ver, kind), fmt.Sprintf("delete a %s", kind), jsonMime, nil, itemParams, "200", "OK", ""),
			}
			paths[itemPath] = filterPathEntry(itemEntry, res, "get")

			// Status subresource
			statusPath := itemPath + "/status"
			paths[statusPath] = map[string]interface{}{
				"get": v2Operation(fmt.Sprintf("read%s%sStatus", ver, kind), fmt.Sprintf("read status of the specified %s", kind), jsonMime, nil, itemParams, "200", "OK", defRef),
				"put": v2Operation(fmt.Sprintf("replace%s%sStatus", ver, kind), fmt.Sprintf("replace status of the specified %s", kind), jsonMime, jsonMime, append(append([]interface{}{}, itemParams...), bodyParam), "200", "OK", defRef),
			}

			// Nested routes for child resources
			if res.ParentResource != nil {
				parentIDParam := map[string]interface{}{"name": "parentID", "in": "path", "required": true, "type": "string", "description": fmt.Sprintf("ID of the parent %s", res.ParentResource.Plural)}
				nestedBase := fmt.Sprintf("/apis/%s/namespaces/{namespace}/%s/{parentID}/%s", gv, res.ParentResource.Plural, res.Plural)
				nestedNSParam := map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"}
				nestedNameParam := map[string]interface{}{"name": "name", "in": "path", "required": true, "type": "string", "description": "name of the resource"}
				for p, entry := range v2NestedResourcePaths(nestedBase, nestedNSParam, parentIDParam, nestedNameParam, kind, res.Plural, res.ParentResource.Plural, defRef) {
					paths[p] = entry
				}
			}

			// Nested routes for child resources with a parent
			if res.ParentResource != nil {
				parentIDParam := map[string]interface{}{
					"name":        "parentID",
					"in":          "path",
					"required":    true,
					"type":        "string",
					"description": fmt.Sprintf("ID of the parent %s", res.ParentResource.Plural),
				}
				nestedBase := fmt.Sprintf("/apis/%s/namespaces/{namespace}/%s/{parentID}/%s", gv, res.ParentResource.Plural, res.Plural)

				paths[nestedBase] = map[string]interface{}{
					"get": map[string]interface{}{
						"description": fmt.Sprintf("list %s for a specific parent", res.Plural),
						"operationId": fmt.Sprintf("listNamespaced%sFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam,
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "OK",
								"schema":      map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
					},
					"post": map[string]interface{}{
						"description": fmt.Sprintf("create a %s under a specific parent", res.GVK.Kind),
						"operationId": fmt.Sprintf("createNamespaced%sFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"consumes":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam,
							map[string]interface{}{
								"name": "body", "in": "body", "required": true,
								"schema": map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
						"responses": map[string]interface{}{
							"201": map[string]interface{}{
								"description": "Created",
								"schema":      map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
					},
				}

				nestedItem := nestedBase + "/{name}"
				nameP := map[string]interface{}{"name": "name", "in": "path", "required": true, "type": "string", "description": "name of the resource"}
				paths[nestedItem] = map[string]interface{}{
					"get": map[string]interface{}{
						"description": fmt.Sprintf("read the specified %s under a specific parent", res.GVK.Kind),
						"operationId": fmt.Sprintf("readNamespaced%sFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam, nameP,
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "OK",
								"schema":      map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
					},
					"put": map[string]interface{}{
						"description": fmt.Sprintf("replace the specified %s under a specific parent", res.GVK.Kind),
						"operationId": fmt.Sprintf("replaceNamespaced%sFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"consumes":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam, nameP,
							map[string]interface{}{
								"name": "body", "in": "body", "required": true,
								"schema": map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "OK",
								"schema":      map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
					},
					"delete": map[string]interface{}{
						"description": fmt.Sprintf("delete a %s under a specific parent", res.GVK.Kind),
						"operationId": fmt.Sprintf("deleteNamespaced%sFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam, nameP,
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{"description": "OK"},
						},
					},
				}

				nestedStatus := nestedItem + "/status"
				paths[nestedStatus] = map[string]interface{}{
					"get": map[string]interface{}{
						"description": fmt.Sprintf("read status of the specified %s under a specific parent", res.GVK.Kind),
						"operationId": fmt.Sprintf("readNamespaced%sStatusFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam, nameP,
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "OK",
								"schema":      map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
					},
					"put": map[string]interface{}{
						"description": fmt.Sprintf("replace status of the specified %s under a specific parent", res.GVK.Kind),
						"operationId": fmt.Sprintf("replaceNamespaced%sStatusFor%s", res.GVK.Kind, res.ParentResource.Plural),
						"produces":    []string{constants.ContentTypeJSON},
						"consumes":    []string{constants.ContentTypeJSON},
						"parameters": []interface{}{
							map[string]interface{}{"name": "namespace", "in": "path", "required": true, "type": "string"},
							parentIDParam, nameP,
							map[string]interface{}{
								"name": "body", "in": "body", "required": true,
								"schema": map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
						"responses": map[string]interface{}{
							"200": map[string]interface{}{
								"description": "OK",
								"schema":      map[string]interface{}{"$ref": fmt.Sprintf("#/definitions/%s", defName)},
							},
						},
					},
				}
			}
		}
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil
	}

	var swagger openapispec.Swagger
	if err := json.Unmarshal(specJSON, &swagger); err != nil {
		return nil
	}

	return &swagger
}

// OpenAPIV2 handles GET /openapi/v2
// Returns the OpenAPI v2 (Swagger 2.0) specification in JSON or protobuf format.
func (h *DiscoveryHandler) OpenAPIV2(w http.ResponseWriter, r *http.Request) {
	// Discovery: GET /openapi/v2
	// Initialize the OpenAPI v2 spec and caches once (lazy initialization)
	h.openAPIV2Once.Do(func() {
		swagger := h.buildOpenAPIV2Spec()
		if swagger == nil {
			return
		}
		h.openAPIV2Spec = swagger

		// Build JSON cache
		jsonData, err := json.Marshal(swagger)
		if err != nil {
			fmt.Printf("[ERROR] Failed to marshal OpenAPI v2 spec to JSON: %v\n", err)
			return
		}
		h.v2JSONCache = jsonData

		// Build protobuf cache using kube-openapi's ToProtoBinary
		protoData, err := openapihandler.ToProtoBinary(jsonData)
		if err != nil {
			// Log error but continue - protobuf support is optional
			return
		}
		h.v2ProtoCache = protoData
	})

	if h.openAPIV2Spec == nil {
		http.Error(w, "Failed to build OpenAPI v2 spec", http.StatusInternalServerError)
		return
	}

	// Parse Accept header to determine response format
	accept := r.Header.Get(constants.HeaderAccept)
	if accept == "" {
		accept = "*/*"
	}

	// Content negotiation
	accepted := []struct {
		Type                string
		SubType             string
		ReturnedContentType string
		Data                []byte
	}{
		{"application", "json", constants.ContentTypeJSON, h.v2JSONCache},
		{"application", "com.github.proto-openapi.spec.v2@v1.0+protobuf", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf", h.v2ProtoCache},
		{"application", "com.github.proto-openapi.spec.v2.v1.0+protobuf", "application/com.github.proto-openapi.spec.v2.v1.0+protobuf", h.v2ProtoCache},
	}

	clauses := goautoneg.ParseAccept(accept)
	w.Header().Add(constants.HeaderVary, constants.HeaderAccept)

	for _, clause := range clauses {
		for _, a := range accepted {
			if (clause.Type == a.Type || clause.Type == "*") &&
				(clause.SubType == a.SubType || clause.SubType == "*") {
				w.Header().Set(constants.HeaderContentType, a.ReturnedContentType)
				w.Header().Set(constants.HeaderLastModified, time.Now().UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusOK)
				w.Write(a.Data)
				return
			}
		}
	}

	// No acceptable format found
	w.WriteHeader(http.StatusNotAcceptable)
}
