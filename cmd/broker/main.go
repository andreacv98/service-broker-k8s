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

package main

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/couchbase/service-broker/pkg/broker"
	"github.com/couchbase/service-broker/pkg/client"
	"github.com/couchbase/service-broker/pkg/config"
	"github.com/couchbase/service-broker/pkg/version"

	"github.com/golang/glog"

	// Import the SQLite3 driver.
	_ "github.com/mattn/go-sqlite3"

	// Import the postgres driver.
	_ "github.com/lib/pq"
)

const (
	// errorCode is what to return on application error.
	errorCode = 1
)

// ErrFatal is raised when the broker is unable to start.
var ErrFatal = errors.New("fatal error")

// authenticationType is the type of authentication the broker should use.
type authenticationType string

const (
	// bearerToken authentication just does a string match.
	bearerToken authenticationType = "token"

	// basic authentication alos does a string match, or username and password.
	// Note Cloud Foundry expects basic auth.
	basic authenticationType = "basic"

	// advanced token authentication uses a JWT token to authenticate.
	advancedToken authenticationType = "advancedToken"
)

// Set sets the authentication type from CLI parameters.
func (a *authenticationType) Set(s string) error {
	switch t := authenticationType(s); t {
	case bearerToken, basic, advancedToken:
		*a = t
	default:
		return fmt.Errorf("%w: unexpected authentication type %s", ErrFatal, s)
	}

	return nil
}

// Type returns the type of flag to display.
func (a *authenticationType) Type() string {
	return "string"
}

// String returns the default authentication type.
func (a *authenticationType) String() string {
	return string(*a)
}

func setupDatabase(stringDbHost, stringDbPort, stringDbUser, stringDbPassword, stringDbName string) (*sql.DB, error) {
	var db *sql.DB
	var err error
	// Check if host or port are empty but credentials are set
	if (stringDbHost == "" || stringDbPort == "") && (stringDbUser != "" || stringDbPassword != "" || stringDbName != "") {
		return db, fmt.Errorf("%w: database host and port must be set if credentials are set", ErrFatal)
	}

	// NO CREDENTIALS SO DEFAULT TO SQLITE3
	// Check if no field is set
	if stringDbHost == "" && stringDbPort == "" && stringDbUser == "" && stringDbPassword == "" && stringDbName == "" {
		// No field is set, use default values to an in memory sqlite3 database
		stringDbHost = "localhost"
		stringDbPort = "5432"

		// Create the in memory database
		db, err = sql.Open("sqlite3", ":memory:")
		if err != nil {
			return db, err
		}
		glog.Infof("Connected to database in memory at %s:%s", stringDbHost, stringDbPort)
	} else if stringDbHost == "" || stringDbPort == "" || stringDbUser == "" || stringDbPassword == "" || stringDbName == "" {
		return db, fmt.Errorf("%w: database host, port, user, password and name must be set", ErrFatal)
	} else {
		// CREDENTIALS ARE SET SO CONNECT TO SQL DATABASE
		// Connect to the database
		db, err = sql.Open("postgres", "host="+stringDbHost+" port="+stringDbPort+" user="+stringDbUser+" password="+stringDbPassword+" dbname="+stringDbName+" sslmode=disable")
		if err != nil {
			return db, err
		}
		glog.Infof("Connected to database at %s:%s", stringDbHost, stringDbPort)
	}
	return db, nil
}

func populateDatabase(db *sql.DB) error {
	var err error
	// Create bought services and plans table with foreign key to users table if it doesn't exist
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS bought_services (id SERIAL PRIMARY KEY, user_id TEXT NOT NULL, service_id TEXT NOT NULL, plan_id TEXT NOT NULL)")
	if err != nil {
		return err
	}
	// Create peering table if it doesn't exist
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS peering (id SERIAL PRIMARY KEY, user_id TEXT NOT NULL, namespace TEXT NOT NULL, cluster_id TEXT NOT NULL, ready BOOLEAN NOT NULL, error TEXT)")
	if err != nil {
		return err
	}
	return nil
}

func main() {
	// authenticationType is the type of authentication to use.
	authentication := basic

	// tokenPath is the location of the file containing the bearer token for authentication.
	var tokenPath string

	// usernamePath is the location of the file containing the username for authentication.
	var usernamePath string

	// passwordPath is the location of the file containing the password for authentication.
	var passwordPath string

	// dbhostPath is the database host for advanced token authentication.
	var dbhostPath string

	// dbportPath is the database port for advanced token authentication.
	var dbportPath string

	// dbuserPath is the database user for advanced token authentication.
	var dbuserPath string

	// dbpasswordPath is the database password for advanced token authentication.
	var dbpasswordPath string

	// dbnamePath is the database name for advanced token authentication.
	var dbnamePath string

	// keycloakURLPath is the location of the file containing the Keycloak URL.
	var keycloakURLPath string

	// keycloakClientIDPath is the location of the file containing the Keycloak Client ID.
	var keycloakClientIDPath string

	// keycloakClientSecretPath is the location of the file containing the Keycloak Client Secret.
	var keycloakClientSecretPath string

	// keycloakRealmPath is the location of the file containing the Keycloak Realm.
	var keycloakRealmPath string

	// tlsCertificatePath is the location of the file containing the TLS server certifcate.
	var tlsCertificatePath string

	// tlsPrivateKeyPath is the location of the file containing the TLS private key.
	var tlsPrivateKeyPath string

	flag.Var(&authentication, "authentication", "Authentication type to use, either 'basic' or 'token'")
	flag.StringVar(&tokenPath, "token", "/var/run/secrets/service-broker/token", "Bearer token for API authentication")
	flag.StringVar(&usernamePath, "username", "/var/run/secrets/service-broker/username", "Username for basic authentication")
	flag.StringVar(&passwordPath, "password", "/var/run/secrets/service-broker/password", "Password for basic authentication")
	flag.StringVar(&dbhostPath, "dbhost", "/var/run/secrets/service-broker/dbhost", "Database host for advanced token authentication")
	flag.StringVar(&dbportPath, "dbport", "/var/run/secrets/service-broker/dbport", "Database port for advanced token authentication")
	flag.StringVar(&dbuserPath, "dbuser", "/var/run/secrets/service-broker/dbuser", "Database user for advanced token authentication")
	flag.StringVar(&dbpasswordPath, "dbpassword", "/var/run/secrets/service-broker/dbpassword", "Database password for advanced token authentication")
	flag.StringVar(&dbnamePath, "dbname", "/var/run/secrets/service-broker/dbname", "Database name for advanced token authentication")
	flag.StringVar(&keycloakURLPath, "keycloak-host", "/var/run/secrets/service-broker/keycloak-host", "Keycloak URL for advanced token authentication")
	flag.StringVar(&keycloakClientIDPath, "keycloak-client-id", "/var/run/secrets/service-broker/keycloak-client-id", "Keycloak client ID for advanced token authentication")
	flag.StringVar(&keycloakClientSecretPath, "keycloak-client-secret", "/var/run/secrets/service-broker/keycloak-client-secret", "Keycloak client secret for advanced token authentication")
	flag.StringVar(&keycloakRealmPath, "keycloak-realm", "/var/run/secrets/service-broker/keycloak-realm", "Keycloak realm for advanced token authentication")
	flag.StringVar(&tlsCertificatePath, "tls-certificate", "/var/run/secrets/service-broker/tls-certificate", "Path to the server TLS certificate")
	flag.StringVar(&tlsPrivateKeyPath, "tls-private-key", "/var/run/secrets/service-broker/tls-private-key", "Path to the server TLS key")
	flag.StringVar(&config.ConfigurationName, "config", config.ConfigurationNameDefault, "Configuration resource name")
	flag.Parse()

	// Start the server.
	glog.Infof("%s %s (git commit %s)", version.Application, version.Version, version.GitCommit)

	c := broker.ServerConfiguration{}

	// Parse implicit configuration.
	namespace, ok := os.LookupEnv("NAMESPACE")
	if !ok {
		glog.Fatal(fmt.Errorf("%w: NAMESPACE environment variable must be set", ErrFatal))
		os.Exit(errorCode)
	}

	c.Namespace = namespace

	// Load up explicit configuration.
	switch authentication {
	case bearerToken:
		glog.Infof("Bearer token authentication")
		token, err := ioutil.ReadFile(tokenPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		stringToken := string(token)
		c.Token = &stringToken

	case basic:
		glog.Infof("Basic authentication")
		username, err := ioutil.ReadFile(usernamePath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		password, err := ioutil.ReadFile(passwordPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		stringUsername := string(username)
		stringPassword := string(password)

		c.BasicAuth = &broker.ServerConfigurationBasicAuth{
			Username: stringUsername,
			Password: stringPassword,
		}

	case advancedToken:
		glog.Infof("Advanced token authentication")

		// Read db data from paths
		dbhost, err := ioutil.ReadFile(dbhostPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read db port from path
		dbport, err := ioutil.ReadFile(dbportPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read db user from path
		dbuser, err := ioutil.ReadFile(dbuserPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read db password from path
		dbpassword, err := ioutil.ReadFile(dbpasswordPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read db name from path
		dbname, err := ioutil.ReadFile(dbnamePath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read keycloak host from path
		keycloakURL, err := ioutil.ReadFile(keycloakURLPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read keycloak client ID from path
		keycloakClientID, err := ioutil.ReadFile(keycloakClientIDPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read keycloak client secret from path
		keycloakClientSecret, err := ioutil.ReadFile(keycloakClientSecretPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Read keycloak realm from path
		keycloakRealm, err := ioutil.ReadFile(keycloakRealmPath)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		stringDbHost := string(dbhost)
		stringDbPort := string(dbport)
		stringDbUser := string(dbuser)
		stringDbPassword := string(dbpassword)
		stringDbName := string(dbname)
		stringKeycloakURL := string(keycloakURL)
		stringKeycloakClientID := string(keycloakClientID)
		stringKeycloakClientSecret := string(keycloakClientSecret)
		stringKeycloakRealm := string(keycloakRealm)

		// Setup Database
		db, err := setupDatabase(stringDbHost, stringDbPort, stringDbUser, stringDbPassword, stringDbName)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Populate database with tables
		err = populateDatabase(db)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		// Setup Keycloak
		client, err := broker.SetupKeycloak(stringKeycloakURL, stringKeycloakClientID, stringKeycloakClientSecret, stringKeycloakRealm)
		if err != nil {
			glog.Fatal(err)
			os.Exit(errorCode)
		}

		c.AdvancedToken = &broker.ServerConfigurationAdvancedToken{
			DatabaseConfiguration: &broker.DatabaseConfiguration{
				DbHost:     stringDbHost,
				DbPort:     stringDbPort,
				DbName:     stringDbName,
				DbUser:     stringDbUser,
				DbPassword: stringDbPassword,
				Db:         db,
			},
			KeycloakConfiguration: &broker.KeycloakConfiguration{
				KeycloakURL:  stringKeycloakURL,
				ClientID:     stringKeycloakClientID,
				ClientSecret: stringKeycloakClientSecret,
				Realm:        stringKeycloakRealm,
				Client:       client,
			},
		}

	}

	cert, err := tls.LoadX509KeyPair(tlsCertificatePath, tlsPrivateKeyPath)
	if err != nil {
		glog.Fatal(err)
		os.Exit(errorCode)
	}

	c.Certificate = cert

	// Initialize the clients.
	clients, err := client.New()
	if err != nil {
		glog.Fatal(err)
		os.Exit(errorCode)
	}

	// Start the server.
	if err := broker.ConfigureServer(clients, &c); err != nil {
		glog.Fatal(err)
		os.Exit(errorCode)
	}

	if err := broker.RunServer(&c); err != nil {
		glog.Fatal(err)
		os.Exit(errorCode)
	}
}
