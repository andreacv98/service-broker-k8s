// Copyright 2020-2021 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file  except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the  License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package broker

import (
	"context"
	"database/sql"
	goerrors "errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	v1 "github.com/couchbase/service-broker/pkg/apis/servicebroker/v1alpha1"
	"github.com/couchbase/service-broker/pkg/api"
	"github.com/couchbase/service-broker/pkg/config"
	"github.com/couchbase/service-broker/pkg/errors"
	"github.com/couchbase/service-broker/pkg/liqo"
	"github.com/couchbase/service-broker/pkg/operation"
	"github.com/couchbase/service-broker/pkg/provisioners"
	"github.com/couchbase/service-broker/pkg/registry"

	"github.com/golang/glog"
	"github.com/julienschmidt/httprouter"

	"k8s.io/apimachinery/pkg/runtime"
)

// ErrUnexpected is highly unlikely to happen...
var ErrUnexpected = goerrors.New("unexpected error")

// handleReadyz is a handler for Kubernetes readiness checks.  It is less verbose than the
// other API calls as it's called significantly more often.
func handleReadyz(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	httpResponse(w, http.StatusOK)
}

// handleReadCatalog advertises the classes of service we offer, and specifc plans to
// implement those classes.
func handleReadCatalog(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	JSONResponse(w, http.StatusOK, config.Config().Spec.Catalog.Convert())
}

func authorizeProvisioning(c *ServerConfiguration, r *http.Request, serviceID, planID string, request *api.CreateServiceInstanceRequest) error {
	// Extract userid from jwt token using gocloak
	tokenString := strings.Split(r.Header.Get("Authorization"), "Bearer ")[1]
	glog.Info("Token: ", tokenString)

	var userID string

	if authorized, err := checkRole(c, tokenString, []string{"manager"});
	err != nil {
		return err
	} else if !authorized {
		authorized, err = checkRole(c, tokenString, []string{"customer"})
		if err != nil {
			return err
		} else if !authorized {
			return fmt.Errorf("user is not authorized to provision services")
		} else {
			// Get userID from token
			userID, err := extractUserId(c, tokenString)
			if err != nil {
				return err
			}
			glog.Info("User ID: ", userID)
		}
	} else {
		// Get userID from body
		// Parse the creation request.
		glog.Info("Request: ", request)
		userID = request.UserID
		if userID == "" {
			return fmt.Errorf("user ID is not provided")
		}
	}

	// Check if user has bought the service
	if err := checkBoughtServices(c.AdvancedToken.DatabaseConfiguration.Db, userID, serviceID, planID); err != nil {
		return err
	}
	glog.Info("User has bought the service")

	// Get servicePlan
	svcPlan, err := getServicePlan(config.Config(), request.ServiceID, request.PlanID)
	if err != nil {
		return err
	}
	// Check the namespace in the context is owned by the user
	destns, err := getNamespace(request.Context, c.Namespace)
	if err != nil {
		return err
	}
	if err := checkNamespaceCompatibility(c, destns, svcPlan, userID); err != nil {
		return err
	}

	return nil
}

func extractUserId(c *ServerConfiguration, tokenString string) (string, error) {
	var userID string
	ctx := context.Background()
	client := c.AdvancedToken.KeycloakConfiguration.Client

	// Wait 500ms
	time.Sleep(1000 * time.Millisecond)
	_, claims, err := client.DecodeAccessToken(
		ctx,
		tokenString,
		c.AdvancedToken.KeycloakConfiguration.Realm,
	)
	if err != nil {
		return userID, err
	}
	glog.Info("Decoded token: ", claims)

	userID = (*claims)["sub"].(string)
	glog.Info("User ID: ", userID)

	return userID, nil
}

func checkBoughtServices(db *sql.DB, userid, serviceID, planID string) error {
	// Create query to check if user has bought the service
	query := "SELECT id FROM bought_services WHERE user_id = $1 AND service_id = $2 AND plan_id = $3"
	// Execute query, only one row should be returned
	row := db.QueryRow(query, userid, serviceID, planID)
	var id string
	err := row.Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("user has not bought the service")
		}
		return err
	}
	return nil
}

func checkRole(c *ServerConfiguration, tokenString string, rolesRequired []string) (bool, error) {
	// Check role of the user
	ctx := context.Background()
	client := c.AdvancedToken.KeycloakConfiguration.Client
	// Extract userid from tokenstring using gocloak
	// Wait 500ms
	userId, err := extractUserId(c, tokenString)
	if err != nil {
		return false, err
	}
	glog.Info("User ID: ", userId)

	// Obtain jwt client
	jwtClient, err := client.LoginClient(
		ctx,
		c.AdvancedToken.KeycloakConfiguration.ClientID,
		c.AdvancedToken.KeycloakConfiguration.ClientSecret,
		c.AdvancedToken.KeycloakConfiguration.Realm,
	)
	if err != nil {
		return false, err
	}
	glog.Info("JWT Client: ", jwtClient)

	// Get role mapping as admin client logged of the user id previously extracted
	time.Sleep(1000 * time.Millisecond)
	roles, err := client.GetRoleMappingByUserID(
		ctx,
		jwtClient.AccessToken,
		c.AdvancedToken.KeycloakConfiguration.Realm,
		userId,
	)
	if err != nil {
		return false, err
	}
	glog.Info("Roles: ", roles)

	// Check if in roles there are all the roles required
	for _, userRole := range *(roles.RealmMappings) {
		if(len(rolesRequired) == 0) {
			// Found all roles required
			break
		}
		for index, roleRequired := range rolesRequired {
			if (*userRole.Name) == roleRequired {
				// Delete roleRequired from rolesRequired
				glog.Info("Found role required: ", roleRequired)
				if(len(roleRequired) == 1) {
					// Delete last element
					rolesRequired = []string{}
					break
				} else {
					// Delete element
					rolesRequired[index] = rolesRequired[len(rolesRequired)-1]
					rolesRequired = rolesRequired[:len(rolesRequired)-1]
				}
			}
		}
	}
	
	// Check lenght of rolesRequired, if it's zero, all roles required are present otherwise return error
	if len(rolesRequired) != 0 {
		return false, nil
	}
	glog.Info("User has the required roles")

	return true, nil
}

func authorizeSubscription(c *ServerConfiguration, r *http.Request, serviceID, planID string) error {
	// Extract token from request
	tokenString := strings.Split(r.Header.Get("Authorization"), "Bearer ")[1]
	glog.Info("Token: ", tokenString)

	// Check role of the user is "manager"
	if authorized, err := checkRole(c, tokenString, []string{"manager"});
	err != nil {
		glog.Error("Error checking role: ", err)
		return err
	} else if !authorized {
		glog.Error("User is not authorized to perform this action")
		return fmt.Errorf("user is not authorized to perform this action")
	}
	glog.Info("User is manager")

	return nil
}

func authorizePeering(c *ServerConfiguration, r *http.Request) error {
	// Extract token from request
	tokenString := strings.Split(r.Header.Get("Authorization"), "Bearer ")[1]
	glog.Info("Token: ", tokenString)
	// Check if the user is a manager
	if authorized, err := checkRole(c, tokenString, []string{"manager"}); err != nil {
		return err
	} else if !authorized {
		return fmt.Errorf("user is not authorized to perform this action")
	}
	glog.Info("User is manager")
	return nil
}

func authorizeGetPeering(c *ServerConfiguration, r *http.Request, peeringUserId string) error {
	// Extract token from request
	tokenString := strings.Split(r.Header.Get("Authorization"), "Bearer ")[1]
	glog.Info("Token: ", tokenString)
	// Check if the user is a manager
	if authorized, err := checkRole(c, tokenString, []string{"manager"}); err != nil {
		return err
	} else if !authorized {
		// User is not a manager, check if the user is the owner of the peering
		userId, err := extractUserId(c, tokenString)
		if err != nil {
			return err
		}
		if userId != peeringUserId {
			return fmt.Errorf("user is not authorized to perform this action")
		}
	}
	glog.Info("User is manager")
	return nil
}


func authorizeUnsubscription(c *ServerConfiguration, r *http.Request) error {
	// Extract token from request
	tokenString := strings.Split(r.Header.Get("Authorization"), "Bearer ")[1]
	glog.Info("Token: ", tokenString)

	// Check role of the user is "manager"
	if authorized, err := checkRole(c, tokenString, []string{"manager"}); err != nil {
		return err
	} else if !authorized {
		return fmt.Errorf("user is not authorized to perform this action")
	}
	glog.Info("User is manager")

	return nil
}

func checkNamespaceCompatibility(c *ServerConfiguration, namespace string, servicePlane *v1.ServicePlan, userID string) error {
	// Check if the namespace is owned by the user
	if err := checkNamespaceOwner(c.AdvancedToken.DatabaseConfiguration.Db, namespace, userID); err != nil {
		return err
	}
	glog.Info("Namespace is owned by the user")

	// Check peeringPolicy is compatible of the servicePlan is compatible with the namespace
	if err := checkPeeringPolicy(c.AdvancedToken.DatabaseConfiguration.Db, namespace, servicePlane); err != nil {
		return err
	}
	glog.Info("Peering policy is compatible")

	return nil
}

func checkNamespaceOwner(db *sql.DB, namespace, userID string) error {
	// Create query to check if namespace is owned by the user
	query := "SELECT namespaces.id FROM namespaces INNER JOIN peering ON namespaces.peering_id = peering.id WHERE namespaces.namespace = $1 AND peering.user_id = $2"
	// Execute query, only one row should be returned
	row := db.QueryRow(query, namespace, userID)
	var id string
	err := row.Scan(&id)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("namespace is not owned by the user")
		}
		return err
	}
	return nil
}

func checkPeeringPolicy(db *sql.DB, namespace string, servicePlane *v1.ServicePlan) error {
	// Get namespaceOffloading based on the namespace
	liqo, err := liqo.Create("liqo")
	if err != nil {
		return err
	}
	namespaceOffloading, err := liqo.GetNamespaceOffloading(namespace)
	if err != nil {
		return err
	}
	glog.Info("Namespace offloading: ", namespaceOffloading)
	glog.Info("Pod offloading strategy of the namespace: " + namespaceOffloading.Spec.PodOffloadingStrategy)

	// Transform namespaceOffloading pod offloading strategy to string
	var nsoffloading = string(namespaceOffloading.Spec.PodOffloadingStrategy)
	glog.Info("Pod offloading strategy of the namespace as STRING: " + nsoffloading)
	// Compare namespaceOffloading exists inside servicePlan.PeeringPolicies
	var found = false
	for _, peeringPolicy := range servicePlane.PeeringPolicies {
		if peeringPolicy.Convert() == nsoffloading {
			found = true
			break;
		}
	}
	if !found {
		return fmt.Errorf("peering policy is not compatible")
	}

	return nil
}

// handleCreateServiceInstance creates a service instance of a plan.
func handleCreateServiceInstance(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		// Ensure the client supports async operation.
		if err := asyncRequired(r); err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Client supports async operation")

		// Parse the creation request.
		request := &api.CreateServiceInstanceRequest{}
		if err := jsonRequest(r, request); err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Request parsed")

		// Check parameters.
		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		if err := validateServicePlan(config.Config(), request.ServiceID, request.PlanID); err != nil {
			jsonError(w, err)
			return
		}

		// Check if the user has bought the service
		if err := authorizeProvisioning(configuration, r, request.ServiceID, request.PlanID, request); err != nil {
			jsonError(w, err)
			return
		}		

		if err := validateParameters(config.Config(), request.ServiceID, request.PlanID, schemaTypeServiceInstance, schemaOperationCreate, request.Parameters); err != nil {
			jsonError(w, err)
			return
		}

		dirent, err := registerDirectoryInstance(config.Config(), request.Context, configuration.Namespace, instanceID, request.ServiceID, request.PlanID)
		if err != nil {
			jsonError(w, err)
			return
		}

		// Check if the instance already exists.
		entry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, false)
		if err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Registry entry created")

		if entry.Exists() {
			glog.Info("Instance already exists")
			// If the instance already exists either return 200 if provisioned or
			// a 202 if it is still provisioning, or a 409 if provisioned or
			// provisioning with different attributes.
			serviceID, ok, err := entry.GetString(registry.ServiceID)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing service ID", ErrUnexpected))
				return
			}

			if serviceID != request.ServiceID {
				jsonError(w, errors.NewResourceConflictError("service ID %s does not match existing value %s", request.ServiceID, serviceID))
				return
			}

			planID, ok, err := entry.GetString(registry.PlanID)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing plan ID", ErrUnexpected))
				return
			}

			if planID != request.PlanID {
				jsonError(w, errors.NewResourceConflictError("plan ID %s does not match existing value %s", request.PlanID, planID))
				return
			}

			context := &runtime.RawExtension{}

			ok, err = entry.Get(registry.Context, context)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing context", ErrUnexpected))
				return
			}

			newContext := &runtime.RawExtension{}
			if request.Context != nil {
				newContext = request.Context
			}

			if !reflect.DeepEqual(newContext, context) {
				jsonError(w, errors.NewResourceConflictError("request context %v does not match existing value %v", newContext, context))
				return
			}

			parameters := &runtime.RawExtension{}

			ok, err = entry.Get(registry.Parameters, parameters)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing parameters", ErrUnexpected))
				return
			}

			newParameters := &runtime.RawExtension{}
			if request.Parameters != nil {
				newParameters = request.Parameters
			}

			if !reflect.DeepEqual(newParameters, parameters) {
				jsonError(w, errors.NewResourceConflictError("request parameters %v do not match existing value %v", newParameters, parameters))
				return
			}

			status := http.StatusOK
			response := &api.CreateServiceInstanceResponse{}

			// There is some ambiguity in the specification, it's accepted if something is already
			// provisioning, or a conflict if it's already provisioning with different parameters,
			// but no mention is made if another operation is in flight e.g. update or deprovision.
			// We'll just call it a conflict.
			operationType, ok, err := entry.GetString(registry.Operation)
			if err != nil {
				jsonError(w, err)
				return
			}

			if ok {
				if operation.Type(operationType) != operation.TypeProvision {
					jsonError(w, errors.NewResourceConflictError("existing %v operation in progress", operationType))
					return
				}

				operationID, ok, err := entry.GetString(registry.OperationID)
				if err != nil {
					jsonError(w, err)
					return
				}

				if !ok {
					jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
					return
				}

				status = http.StatusAccepted
				response.Operation = operationID
			}

			dashboardURL, ok, err := entry.GetString(registry.DashboardURL)
			if err != nil {
				jsonError(w, err)
				return
			}

			if ok {
				response.DashboardURL = dashboardURL
			}

			JSONResponse(w, status, response)

			return
		}

		glog.Info("Creating new instance")
		context := &runtime.RawExtension{}
		if request.Context != nil {
			context = request.Context
		}
		glog.Infof("Context: %v", context)

		parameters := &runtime.RawExtension{}
		if request.Parameters != nil {
			parameters = request.Parameters
		}
		glog.Infof("Parameters: %v", parameters)

		namespace, err := getNamespace(request.Context, configuration.Namespace)
		if err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Namespace: ", namespace)

		if err := entry.Set(registry.Namespace, namespace); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.InstanceID, instanceID); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.ServiceID, request.ServiceID); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.PlanID, request.PlanID); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.Context, context); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.Parameters, parameters); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Commit(); err != nil {
			jsonError(w, err)
			return
		}

		glog.Infof("provisioning new service instance: %s", instanceID)

		// Create a provisioning engine, and perform synchronous tasks.  This also derives
		// things like the dashboard URL for the synchronous response.
		provisioner, err := provisioners.NewCreator(provisioners.ResourceTypeServiceInstance)
		if err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Provisioner: ", provisioner)

		if err := provisioner.Prepare(entry); err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Prepared")

		if err := operation.Start(entry, operation.TypeProvision); err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("Started")

		frozenEntry := entry.Clone()

		go provisioner.Run(entry)

		glog.Info("Running")

		operationID, ok, err := frozenEntry.GetString(registry.OperationID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
			return
		}

		glog.Info("Operation ID: ", operationID)

		// Return a response to the client.
		response := &api.CreateServiceInstanceResponse{
			Operation: operationID,
		}

		dashboardURL, ok, err := frozenEntry.GetString(registry.DashboardURL)
		if err != nil {
			jsonError(w, err)
			return
		}

		if ok {
			response.DashboardURL = dashboardURL
		}

		JSONResponse(w, http.StatusAccepted, response)
	}
}

// handleReadServiceInstance allows a service instance to be read.
func handleReadServiceInstance(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		dirent := getDirectoryInstance(configuration.Namespace, instanceID)

		// Check if the instance exists.
		entry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, true)
		if err != nil {
			jsonError(w, err)
			return
		}

		// Not found, return a 404
		if !entry.Exists() {
			jsonError(w, errors.NewResourceNotFoundError("service instance does not exist"))
			return
		}

		// service_id is optional and provoded as a hint.
		serviceID, serviceIDProvided, err := maygetSingleParameter(r, "service_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		// plan_id is optional and provoded as a hint.
		planID, planIDProvided, err := maygetSingleParameter(r, "plan_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		serviceInstanceServiceID, ok, err := entry.GetString(registry.ServiceID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing service ID", ErrUnexpected))
			return
		}

		serviceInstancePlanID, ok, err := entry.GetString(registry.PlanID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing plan ID", ErrUnexpected))
			return
		}

		if serviceIDProvided && serviceID != serviceInstanceServiceID {
			jsonError(w, errors.NewQueryError("specified service ID %s does not match %s", serviceID, serviceInstanceServiceID))
			return
		}

		if planIDProvided && planID != serviceInstancePlanID {
			jsonError(w, errors.NewQueryError("specified plan ID %s does not match %s", planID, serviceInstancePlanID))
			return
		}

		parameters := &runtime.RawExtension{}

		ok, err = entry.Get(registry.Parameters, parameters)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing parameters", ErrUnexpected))
			return
		}

		// If the instance does not exist or an operation is still in progress return
		// a 404.
		op, ok, err := entry.GetString(registry.Operation)
		if err != nil {
			jsonError(w, err)
			return
		}

		if ok {
			jsonError(w, errors.NewParameterError("%s operation in progress", op))
			return
		}

		response := &api.GetServiceInstanceResponse{
			ServiceID:  serviceInstanceServiceID,
			PlanID:     serviceInstancePlanID,
			Parameters: parameters,
		}
		JSONResponse(w, http.StatusOK, response)
	}
}

// handleUpdateServiceInstance allows a service instance to be modified.
func handleUpdateServiceInstance(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		// Ensure the client supports async operation.
		if err := asyncRequired(r); err != nil {
			jsonError(w, err)
			return
		}

		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonErrorUsable(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		// Parse the update request.
		request := &api.UpdateServiceInstanceRequest{}
		if err := jsonRequest(r, request); err != nil {
			jsonError(w, err)
			return
		}

		dirent := getDirectoryInstance(configuration.Namespace, instanceID)

		// Check if the instance already exists.
		// Check if the instance exists.
		entry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, false)
		if err != nil {
			jsonError(w, err)
			return
		}

		// Not found, return a 404
		if !entry.Exists() {
			jsonError(w, errors.NewResourceNotFoundError("service instance does not exist"))
			return
		}

		// Get the plan from the registry, it is not guaranteed to be in the request.
		// Override with the request if specified.
		planID, ok, err := entry.GetString(registry.PlanID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing plan ID", ErrUnexpected))
			return
		}

		newPlanID := planID
		if request.PlanID != "" {
			newPlanID = request.PlanID
		}

		// Check parameters.
		if err := validateServicePlan(config.Config(), request.ServiceID, newPlanID); err != nil {
			jsonError(w, err)
			return
		}

		if err := planUpdatable(config.Config(), request.ServiceID, planID, newPlanID); err != nil {
			jsonError(w, err)
			return
		}

		if err := validateParameters(config.Config(), request.ServiceID, planID, schemaTypeServiceInstance, schemaOperationUpdate, request.Parameters); err != nil {
			jsonErrorUsable(w, err)
			return
		}

		parameters := &runtime.RawExtension{}
		if request.Parameters != nil {
			parameters = request.Parameters
		}

		if err := entry.Set(registry.Parameters, parameters); err != nil {
			jsonError(w, err)
			return
		}

		updater, err := provisioners.NewUpdater(provisioners.ResourceTypeServiceInstance, request)
		if err != nil {
			jsonErrorUsable(w, err)
			return
		}

		if err := updater.Prepare(entry); err != nil {
			jsonErrorUsable(w, err)
			return
		}

		if err := operation.Start(entry, operation.TypeUpdate); err != nil {
			jsonError(w, err)
			return
		}

		frozenEntry := entry.Clone()

		go updater.Run(entry)

		operationID, ok, err := frozenEntry.GetString(registry.OperationID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
		}

		// Return a response to the client.
		response := &api.UpdateServiceInstanceResponse{
			Operation: operationID,
		}

		JSONResponse(w, http.StatusAccepted, response)
	}
}

// handleDeleteServiceInstance deletes a service instance.
func handleDeleteServiceInstance(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		// Ensure the client supports async operation.
		if err := asyncRequired(r); err != nil {
			jsonError(w, err)
			return
		}

		// Check parameters.
		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		dirent := getDirectoryInstance(configuration.Namespace, instanceID)

		// Probably the wrong place for this...
		deleteDirectoryInstance(configuration.Namespace, instanceID)

		entry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, false)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !entry.Exists() {
			jsonError(w, errors.NewResourceGoneError("service instance does not exist"))
			return
		}

		serviceID, err := getSingleParameter(r, "service_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		planID, err := getSingleParameter(r, "plan_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		serviceInstanceServiceID, ok, err := entry.GetString(registry.ServiceID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing service ID", ErrUnexpected))
			return
		}

		serviceInstancePlanID, ok, err := entry.GetString(registry.PlanID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing plan ID", ErrUnexpected))
			return
		}

		if serviceID != serviceInstanceServiceID {
			jsonError(w, errors.NewQueryError("specified service ID %s does not match %s", serviceID, serviceInstanceServiceID))
			return
		}

		if planID != serviceInstancePlanID {
			jsonError(w, errors.NewQueryError("specified plan ID %s does not match %s", planID, serviceInstancePlanID))
			return
		}

		deleter := provisioners.NewDeleter()

		// Start the delete operation in the background.
		if err := operation.Start(entry, operation.TypeDeprovision); err != nil {
			jsonError(w, err)
			return
		}

		go deleter.Run(entry)

		operationID, ok, err := entry.GetString(registry.OperationID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
		}

		response := &api.CreateServiceInstanceResponse{
			Operation: operationID,
		}
		JSONResponse(w, http.StatusAccepted, response)
	}
}

// handlePollServiceInstance polls a service instance operation for status.
func handlePollServiceInstance(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		dirent := getDirectoryInstance(configuration.Namespace, instanceID)

		entry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, false)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !entry.Exists() {
			JSONResponse(w, http.StatusGone, struct{}{})
			return
		}

		// service_id is optional and provoded as a hint.
		serviceID, serviceIDProvided, err := maygetSingleParameter(r, "service_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		// plan_id is optional and provided as a hint.
		planID, planIDProvided, err := maygetSingleParameter(r, "plan_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		// operation is optional, however the broker only implements asynchronous
		// operations at present, so require it unconditionally.
		operationID, err := getSingleParameter(r, "operation")
		if err != nil {
			jsonError(w, err)
			return
		}

		instanceServiceID, ok, err := entry.GetString(registry.ServiceID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
		}

		instancePlanID, ok, err := entry.GetString(registry.PlanID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
		}

		instanceOperationID, ok, err := entry.GetString(registry.OperationID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
		}

		// While not specified, we check that the provided service ID matches the one
		// we expect.  It may be indicative of a client error.
		if serviceIDProvided && serviceID != instanceServiceID {
			jsonError(w, errors.NewQueryError("provided service ID %s does not match %s", serviceID, instanceServiceID))
			return
		}

		// While not specified, we check that the provided plan ID matches the one
		// we expect.  It may be indicative of a client error.
		if planIDProvided && planID != instancePlanID {
			jsonError(w, errors.NewQueryError("provided plan ID %s does not match %s", planID, instancePlanID))
			return
		}

		if operationID != instanceOperationID {
			jsonError(w, errors.NewQueryError("provided operation %s does not match operation %s", operationID, instanceOperationID))
			return
		}

		operationStatus, ok, err := entry.GetString(registry.OperationStatus)
		if err != nil {
			jsonError(w, err)
			return
		}

		// If there is no status then the provisioning operation is still in progress (or has crashed...)
		if !ok {
			response := &api.PollServiceInstanceResponse{
				State:       api.PollStateInProgress,
				Description: "asynchronous provisioning in progress",
			}
			JSONResponse(w, http.StatusOK, response)

			return
		}

		// If the status isn't empty then we have encountered an error and need to report failure.
		if operationStatus != "" {
			if err := operation.End(entry); err != nil {
				jsonError(w, err)
				return
			}

			response := &api.PollServiceInstanceResponse{
				State:       api.PollStateFailed,
				Description: operationStatus,
			}
			JSONResponse(w, http.StatusOK, response)

			return
		}

		// All checks have passed, instance successfully provisioned.
		if err := operation.End(entry); err != nil {
			jsonError(w, err)
			return
		}

		response := &api.PollServiceInstanceResponse{
			State: api.PollStateSucceeded,
		}
		JSONResponse(w, http.StatusOK, response)
	}
}

// handleCreateServiceBinding creates a binding to a service instance.
func handleCreateServiceBinding(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		// Parse the creation request.
		request := &api.CreateServiceBindingRequest{}
		if err := jsonRequest(r, request); err != nil {
			jsonError(w, err)
			return
		}

		// Check parameters.
		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		bindingID := params.ByName("binding_id")
		if bindingID == "" {
			jsonError(w, fmt.Errorf("%w: request missing binding_id parameter", ErrUnexpected))
			return
		}

		if err := validateServicePlan(config.Config(), request.ServiceID, request.PlanID); err != nil {
			jsonError(w, err)
			return
		}

		if err := verifyBindable(config.Config(), request.ServiceID, request.PlanID); err != nil {
			jsonError(w, err)
			return
		}

		if err := validateParameters(config.Config(), request.ServiceID, request.PlanID, schemaTypeServiceBinding, schemaOperationCreate, request.Parameters); err != nil {
			jsonError(w, err)
			return
		}

		// Check if the service instance exists.
		dirent := getDirectoryInstance(configuration.Namespace, instanceID)

		instanceEntry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, true)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !instanceEntry.Exists() {
			jsonError(w, errors.NewParameterError("service instance %s not found", instanceID))
			return
		}

		// Check if the binding already exists.
		entry, err := registry.New(registry.ServiceBinding, dirent.Namespace, bindingID, false)
		if err != nil {
			jsonError(w, err)
			return
		}

		if entry.Exists() {
			// If the binding already exists either return 200 if provisioned or
			// a 202 if it is still provisioning, or a 409 if provisioned or
			// provisioning with different attributes.
			serviceID, ok, err := entry.GetString(registry.ServiceID)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing service ID", ErrUnexpected))
				return
			}

			if serviceID != request.ServiceID {
				jsonError(w, errors.NewResourceConflictError("service ID %s does not match existing value %s", request.ServiceID, serviceID))
				return
			}

			planID, ok, err := entry.GetString(registry.PlanID)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing plan ID", ErrUnexpected))
				return
			}

			if planID != request.PlanID {
				jsonError(w, errors.NewResourceConflictError("plan ID %s does not match existing value %s", request.PlanID, planID))
				return
			}

			context := &runtime.RawExtension{}

			ok, err = entry.Get(registry.Context, context)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing context", ErrUnexpected))
				return
			}

			newContext := &runtime.RawExtension{}
			if request.Context != nil {
				newContext = request.Context
			}

			if !reflect.DeepEqual(newContext, context) {
				jsonError(w, errors.NewResourceConflictError("request context %v does not match existing value %v", newContext, context))
				return
			}

			parameters := &runtime.RawExtension{}

			ok, err = entry.Get(registry.Parameters, parameters)
			if err != nil {
				jsonError(w, err)
				return
			}

			if !ok {
				jsonError(w, fmt.Errorf("%w: unable to lookup existing parameters", ErrUnexpected))
				return
			}

			newParameters := &runtime.RawExtension{}
			if request.Parameters != nil {
				newParameters = request.Parameters
			}

			if !reflect.DeepEqual(newParameters, parameters) {
				jsonError(w, errors.NewResourceConflictError("request parameters %v do not match existing value %v", newParameters, parameters))
				return
			}

			status := http.StatusOK
			response := &api.CreateServiceBindingResponse{}

			// There is some ambiguity in the specification, it's accepted if something is already
			// provisioning, or a conflict if it's already provisioning with different parameters,
			// but no mention is made if another operation is in flight e.g. update or deprovision.
			// We'll just call it a conflict.
			operationType, ok, err := entry.GetString(registry.Operation)
			if err != nil {
				jsonError(w, err)
				return
			}

			if ok {
				if operation.Type(operationType) != operation.TypeProvision {
					jsonError(w, errors.NewResourceConflictError("existing %v operation in progress", operationType))
					return
				}

				operationID, ok, err := entry.GetString(registry.OperationID)
				if err != nil {
					jsonError(w, err)
					return
				}

				if !ok {
					jsonError(w, fmt.Errorf("%w: service instance missing operation ID", ErrUnexpected))
					return
				}

				response.Operation = operationID
			}

			JSONResponse(w, status, response)

			return
		}

		// The binding gets a copy of all service instance data, this could be used
		// to communicate TLS or other password information.  The context and parameters
		// are overridden buy those related to the binding.
		entry.Inherit(instanceEntry)

		context := &runtime.RawExtension{}
		if request.Context != nil {
			context = request.Context
		}

		parameters := &runtime.RawExtension{}
		if request.Parameters != nil {
			parameters = request.Parameters
		}

		if err := entry.Set(registry.BindingID, bindingID); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.Context, context); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Set(registry.Parameters, parameters); err != nil {
			jsonError(w, err)
			return
		}

		if err := entry.Commit(); err != nil {
			jsonError(w, err)
			return
		}

		glog.Infof("provisioning new service binding: %s", bindingID)

		// Create a provisioning engine, and perform synchronous tasks.  This also derives
		// things like the dashboard URL for the synchronous response.
		provisioner, err := provisioners.NewCreator(provisioners.ResourceTypeServiceBinding)
		if err != nil {
			jsonError(w, err)
			return
		}

		if err := provisioner.Prepare(entry); err != nil {
			jsonError(w, err)
			return
		}

		if err := operation.Start(entry, operation.TypeProvision); err != nil {
			jsonError(w, err)
			return
		}

		frozenEntry := entry.Clone()

		provisioner.Run(entry)

		operationStatus, ok, err := entry.GetString(registry.OperationStatus)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: expected operation status not found", ErrUnexpected))
			return
		}

		// Stop the operation to allow other things to happen now.
		if err := operation.End(entry); err != nil {
			jsonError(w, err)
			return
		}

		if operationStatus != "" {
			// Work needed: properly propagate the error type.
			jsonError(w, errors.NewConfigurationError(operationStatus))
			return
		}

		credentials := &runtime.RawExtension{}

		if _, err := frozenEntry.Get(registry.Credentials, credentials); err != nil {
			jsonError(w, err)
			return
		}

		response := &api.GetServiceBindingResponse{
			Credentials: credentials,
		}
		JSONResponse(w, http.StatusCreated, response)
	}
}

// handleDeleteServiceBinding deletes a service binding.
func handleDeleteServiceBinding(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		instanceID := params.ByName("instance_id")
		if instanceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing instance_id parameter", ErrUnexpected))
			return
		}

		dirent := getDirectoryInstance(configuration.Namespace, instanceID)

		instanceEntry, err := registry.New(registry.ServiceInstance, dirent.Namespace, instanceID, true)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !instanceEntry.Exists() {
			jsonError(w, errors.NewParameterError("service instance %s not found", instanceID))
			return
		}

		// Check parameters.
		bindingID := params.ByName("binding_id")
		if bindingID == "" {
			jsonError(w, fmt.Errorf("%w: request missing binding_id parameter", ErrUnexpected))
			return
		}

		entry, err := registry.New(registry.ServiceBinding, dirent.Namespace, bindingID, false)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !entry.Exists() {
			jsonError(w, errors.NewResourceGoneError("service instance does not exist"))
			return
		}

		serviceID, err := getSingleParameter(r, "service_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		planID, err := getSingleParameter(r, "plan_id")
		if err != nil {
			jsonError(w, err)
			return
		}

		serviceInstanceServiceID, ok, err := entry.GetString(registry.ServiceID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing service ID", ErrUnexpected))
			return
		}

		serviceInstancePlanID, ok, err := entry.GetString(registry.PlanID)
		if err != nil {
			jsonError(w, err)
			return
		}

		if !ok {
			jsonError(w, fmt.Errorf("%w: unable to lookup existing plan ID", ErrUnexpected))
			return
		}

		if serviceID != serviceInstanceServiceID {
			jsonError(w, errors.NewQueryError("specified service ID %s does not match %s", serviceID, serviceInstanceServiceID))
			return
		}

		if planID != serviceInstancePlanID {
			jsonError(w, errors.NewQueryError("specified plan ID %s does not match %s", planID, serviceInstancePlanID))
			return
		}

		deleter := provisioners.NewDeleter()

		deleter.Run(entry)

		response := &api.DeleteServiceBindingResponse{}
		JSONResponse(w, http.StatusOK, response)
	}
}

// handleServiceSubscription buys a service.
func handleServiceSubscription(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {

		// Parse the request body.
		request := &api.ServiceSubscriptionRequest{}
		if err := jsonRequest(r, request); err != nil {
			jsonError(w, err)
			return
		}

		// Get service_id and plan_id from the request body
		serviceID := request.ServiceID
		if serviceID == "" {
			jsonError(w, fmt.Errorf("%w: request missing service_id parameter", ErrUnexpected))
			return
		}

		planID := request.PlanID
		if planID == "" {
			jsonError(w, fmt.Errorf("%w: request missing plan_id parameter", ErrUnexpected))
			return
		}

		// validate service plan
		if err := validateServicePlan(config.Config(), serviceID, planID); err != nil {
			jsonError(w, err)
			return
		}

		// Get user_id and namespace from the request body
		userID := request.UserID
		if userID == "" {
			jsonError(w, fmt.Errorf("%w: request missing user_id parameter", ErrUnexpected))
			return
		}

		glog.Info("Request to buy service: ", serviceID, " plan: ", planID, " for user: ", userID)

		// Check if the user who request the purchase can buy the service
		// TLDR: the user is a manager
		if err := authorizeSubscription(configuration, r, serviceID, planID); err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("user authorized to buy the service")

		// Check if the user is already registred with the specified namespace in the database
		db := configuration.AdvancedToken.DatabaseConfiguration.Db

		// Check if the user has already bought the service
		rows, err := db.Query("SELECT * FROM bought_services WHERE bought_services.service_id = $1 AND bought_services.plan_id = $2 AND bought_services.user_id = $3", serviceID, planID, userID)
		if err != nil {
			jsonError(w, err)
			return
		}
		defer rows.Close()
		if rows.Next() {
			jsonError(w, errors.NewResourceGoneError("service already bought"))
			return
		}
		glog.Info("service not already bought")

		// Insert the purchase in the database
		row := db.QueryRow("INSERT INTO bought_services (service_id, plan_id, user_id) VALUES ($1, $2, $3) RETURNING id", serviceID, planID, userID)

		// Get the purchase id
		var subscriptionID int
		err = row.Scan(&subscriptionID)
		if err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("purchaseID: ", subscriptionID)

		response := &api.ServiceSubscriptionResponse{
			SubscriptionID: strconv.Itoa(int(subscriptionID)),
		}
		JSONResponse(w, http.StatusOK, response)
	}
}

// handleServiceUnsubscription deletes a service subscription.
func handleDeleteServiceSubscription(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {

		// Parse the request body.
		request := &api.ServiceUnsubscriptionRequest{}
		if err := jsonRequest(r, request); err != nil {
			jsonError(w, err)
			return
		}

		// Get subscription_id from the request body
		subscriptionID := request.SubscriptionID
		if subscriptionID == "" {
			jsonError(w, fmt.Errorf("%w: request missing subscription_id parameter", ErrUnexpected))
			return
		}

		glog.Info("Request to delete service subscription: ", subscriptionID)

		// Check if the user who request the deletion of the subscription is authorized
		if err := authorizeUnsubscription(configuration, r); err != nil {
			jsonError(w, err)
			return
		}
		glog.Info("user authorized to delete the service subscription")

		// TODO: delete the subscription from the database
		// TODO: delete the service bindings assosciated to the service_id and plan_id extracted by the subscription_id from the cluster
		// TODO: delete the service instances assosciated to the service_id and plan_id extracted by the subscription_id from the cluster
	}
}

func handlePeering(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		glog.Info("handlePeering")
		request := &api.PeeringRequest{}
		if err := jsonRequest(r, request); err != nil {
			glog.Info("Error in jsonRequest: ", err)
			jsonError(w, err)
			return
		}

		glog.Info("Request to create peering: ", request)

		// Check if the user who request the peering is authorized
		err := authorizePeering(configuration, r)
		if err != nil {
			glog.Info("Error in authorizePeering: ", err)
			JSONResponse(w, http.StatusUnauthorized, err)
			return
		}

		// Create the liqo struct
		liqo, err := liqo.Create("liqo")
		if err != nil {
			glog.Info("Error in liqo.Create: ", err)
			jsonError(w, err)
			return
		}

		ctx := context.Background()

		// Check not already peered
		row := configuration.AdvancedToken.DatabaseConfiguration.Db.QueryRow("SELECT id, user_id FROM peering WHERE cluster_id = $1", request.ClusterID)
		var peeringid int
		var user_id string
		err = row.Scan(&peeringid, &user_id)
		if err == nil {
			// Peering already exists
			glog.Info("Already peered with cluster: ", request.ClusterID)
			// Check if the peering is owned by the user in the request
			if user_id != request.UserID {
				glog.Info("Peering already exists and is owned by another user")
				// Return conflict code with empty body
				JSONResponse(w, http.StatusConflict, nil)
			} else {
				glog.Info("Peering already exists and is owned by the user")
				// Register namespace associated to the peering into db
				row = configuration.AdvancedToken.DatabaseConfiguration.Db.QueryRow("INSERT INTO namespaces (peering_id, namespace, ready) VALUES ($1, $2, $3) RETURNING id", peeringid, request.PrefixNamespace, false)
				// Get the ID
				var namespace_id int
				err = row.Scan(&namespace_id)
				if err != nil {
					glog.Info("Error while scanning namespace_id: ", err)
					jsonError(w, err)
					return
				}
				// Liqo create namespace and offload it
				// TODO: sync or async operation?
				go liqo.NamespaceAndOffload(
					ctx,
					configuration.AdvancedToken.DatabaseConfiguration.Db,
					peeringid,
					namespace_id,
					request.ClusterID,
					request.ClusterName,
					request.OffloadingPolicy,
					request.PrefixNamespace,
				)
				glog.Info("Peering created with id: ", peeringid, " and namespaceId: " , namespace_id)
				response := &api.PeeringResponse{
					PeeringID: strconv.Itoa(namespace_id),
				}
				JSONResponse(w, http.StatusAccepted, response)
			}
			return
		}
		if err != sql.ErrNoRows {
			glog.Info("Error while scanning peeringid: ", err)
			jsonError(w, err)
			return
		}
		// Register peering into db
		row = configuration.AdvancedToken.DatabaseConfiguration.Db.QueryRow("INSERT INTO peering (cluster_id, ready, user_id) VALUES ($1, $2, $3) RETURNING id", request.ClusterID, false, request.UserID)
		// Get the ID
		err = row.Scan(&peeringid)
		if err != nil {
			glog.Info("Error while scanning peeringid: ", err)
			jsonError(w, err)
			return
		}

		// Register namespace associated to the peering into db
		row = configuration.AdvancedToken.DatabaseConfiguration.Db.QueryRow("INSERT INTO namespaces (peering_id, namespace, ready) VALUES ($1, $2, $3) RETURNING id", peeringid, request.PrefixNamespace, false)
		// Get the ID
		var namespace_id int
		err = row.Scan(&namespace_id)
		if err != nil {
			glog.Info("Error while scanning namespace_id: ", err)
			jsonError(w, err)
			return
		}

		// Start goroutine to create peering and offload namespace
		go liqo.PeerAndNamespace(
			ctx,
			request.ClusterID,
			request.Token,
			request.AuthURL,
			request.ClusterName,
			request.OffloadingPolicy,
			request.UserID,
			request.PrefixNamespace,
			peeringid,
			namespace_id,
			configuration.AdvancedToken.DatabaseConfiguration.Db,
		)

		glog.Info("Peering created with id: ", peeringid, " and namespaceId: " , namespace_id)
		response := &api.PeeringResponse{
			PeeringID: strconv.Itoa(namespace_id),
		}
		glog.Info("Peering response: ", response)
		JSONResponse(w, http.StatusAccepted, response)
	}
}

func handleCheckPeeringStatus(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		glog.Info("handleCheckPeeringStatus")

		// Check into db peering status through request.PeeringID
		// Get peering id from the url
		peeringID := params.ByName("peering_id")

		glog.Info("Request to check peering status for peeringID: ", peeringID)
		db := configuration.AdvancedToken.DatabaseConfiguration.Db
		row := db.QueryRow("SELECT peering.ready, peering.error, namespaces.namespace, namespaces.ready, namespaces.error, peering.user_id FROM peering INNER JOIN namespaces ON peering.id = namespaces.peering_id WHERE namespaces.id = $1", peeringID)
		var ready bool
		var error sql.NullString
		var namespace string
		var readyNs bool
		var errorNs sql.NullString
		var user_id string
		err := row.Scan(&ready, &error, &namespace, &readyNs, &errorNs, &user_id)
		if err != nil {
			// Check if query return any result
			if err == sql.ErrNoRows {
				glog.Info("No result found for peeringID: ", peeringID)
				// Return bad request
				response := &api.CheckPeeringStatusResponseNotReady{
					Ready: false,
					Error: "No result found for peeringID: " + peeringID,
				}
				JSONResponse(w, http.StatusBadRequest, response)
				return
			}
			glog.Info("Error while scanning ready and error: ", err)
			jsonError(w, err)
			return
		}

		// Check if the user who request the peering is authorized
		err = authorizeGetPeering(configuration, r, user_id)
		if err != nil {
			glog.Info("Error in authorizeGetPeering: ", err)
			JSONResponse(w, http.StatusUnauthorized, err)
			return
		}

		if ready && readyNs {
			// Peering is ready
			response := &api.CheckPeeringStatusResponseNamespace{
				Namespace: namespace,
			}
			JSONResponse(w, http.StatusOK, response)
		} else {
			if(!ready) {
				// Peering is not ready
				if !(error.Valid) {
					// Peering is ongoing
					glog.Info("Peering is ongoing")
					response := &api.CheckPeeringStatusResponseNotReady{
						Ready: false,
					}
					JSONResponse(w, http.StatusAccepted, response)
					return
				} else {
					// Peering failed
					glog.Info("Peering failed")
					response := &api.CheckPeeringStatusResponseNotReady{
						Ready: false,
						Error: error.String,
					}
					JSONResponse(w, http.StatusInternalServerError, response)
					return
				}
			} else if (!readyNs) {
				// Namespace is not ready
				if !(errorNs.Valid) {
					// Namespace is ongoing
					glog.Info("Namespace is ongoing")
					response := &api.CheckPeeringStatusResponseNotReady{
						Ready: false,
					}
					JSONResponse(w, http.StatusAccepted, response)
					return
				} else {
					// Namespace failed
					glog.Info("Namespace failed")
					response := &api.CheckPeeringStatusResponseNotReady{
						Ready: false,
						Error: errorNs.String,
					}
					JSONResponse(w, http.StatusInternalServerError, response)
					return
				}
			}
			
		}

	}	
}

func handleCreateCredentials(configuration *ServerConfiguration) func(http.ResponseWriter, *http.Request, httprouter.Params) {
	return func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
		glog.Info("handleCreateCredentials")
		request := &api.CreateCredentialsRequest{}
		if err := jsonRequest(r, request); err != nil {
			glog.Info("Error in jsonRequest: ", err)
			jsonError(w, err)
			return
		}

		glog.Info("Request to create credentials: ", request)		

		// Create keycloak client
		client, err := SetupKeycloak(request.AuthURL, request.ClientID, request.ClientSecret, request.Realm)
		if err != nil {
			glog.Info("Error in SetupKeycloak: ", err)
			jsonError(w, err)
			return
		}

		if client == nil {
			glog.Info("Client not created")
			JSONResponse(w, http.StatusInternalServerError, "OIDC client not created")
			return
		}

		// Set field of configuration keycloak
		configuration.AdvancedToken.KeycloakConfiguration.KeycloakURL = request.AuthURL
		configuration.AdvancedToken.KeycloakConfiguration.ClientID = request.ClientID
		configuration.AdvancedToken.KeycloakConfiguration.ClientSecret = request.ClientSecret
		configuration.AdvancedToken.KeycloakConfiguration.Realm = request.Realm
		configuration.AdvancedToken.KeycloakConfiguration.Client = client

		response := &api.CreateCredentialsResponse{}
		JSONResponse(w, http.StatusCreated, response)
	}
}