/*
 * Copyright (c) "Neo4j"
 * Neo4j Sweden AB [https://neo4j.com]
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package db defines generic database functionality.
package db

import (
	"context"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/auth"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/db"
	iauth "github.com/neo4j/neo4j-go-driver/v5/neo4j/internal/auth"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/internal/telemetry"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/log"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/notifications"
	"math"
	"time"
)

// Definitions of these should correspond to public API
type AccessMode int

const (
	WriteMode AccessMode = 0
	ReadMode  AccessMode = 1
)

type (
	TxHandle     uint64
	StreamHandle any
)

type Command struct {
	Cypher    string
	Params    map[string]any
	FetchSize int
}

type TxConfig struct {
	Mode               AccessMode
	Bookmarks          []string
	Timeout            time.Duration
	ImpersonatedUser   string
	Meta               map[string]any
	NotificationConfig NotificationConfig
}

type NotificationConfig struct {
	MinSev  notifications.NotificationMinimumSeverityLevel
	DisCats notifications.NotificationDisabledCategories
	DisClas notifications.NotificationDisabledClassifications
}

func (n *NotificationConfig) ToMeta(meta map[string]any, version db.ProtocolVersion) {
	if n.MinSev != notifications.DefaultLevel {
		meta["notifications_minimum_severity"] = string(n.MinSev)
	}

	disabledKey := "notifications_disabled_categories"
	if version.Minor >= 5 {
		disabledKey = "notifications_disabled_classifications"
	}
	if n.DisCats.DisablesNone() || n.DisClas.DisablesNone() {
		meta[disabledKey] = make([]string, 0)
	} else {
		if len(n.DisCats.DisabledCategories()) > 0 {
			meta[disabledKey] = n.DisCats.DisabledCategories()
		}
		if len(n.DisClas.DisabledClassifications()) > 0 {
			meta[disabledKey] = n.DisClas.DisabledClassifications()
		}
	}
}

const DefaultTxConfigTimeout = math.MinInt

// Connection defines an abstract database server connection.
type Connection interface {
	Connect(
		ctx context.Context,
		minor int,
		auth *ReAuthToken,
		userAgent string,
		routingContext map[string]string,
		notificationConfig NotificationConfig,
	) error

	TxBegin(ctx context.Context, txConfig TxConfig, syncMessages bool) (TxHandle, error)
	TxRollback(ctx context.Context, tx TxHandle) error
	TxCommit(ctx context.Context, tx TxHandle) error
	Run(ctx context.Context, cmd Command, txConfig TxConfig) (StreamHandle, error)
	RunTx(ctx context.Context, tx TxHandle, cmd Command) (StreamHandle, error)
	// Keys for the specified stream.
	Keys(streamHandle StreamHandle) ([]string, error)
	// Next moves to next item in the stream.
	// If error is nil, either Record or Summary has a value, if Record is nil there are no more records.
	// If error is non nil, neither Record or Summary has a value.
	Next(ctx context.Context, streamHandle StreamHandle) (*db.Record, *db.Summary, error)
	// Consume discards all records on the stream and returns the summary otherwise it will return the error.
	Consume(ctx context.Context, streamHandle StreamHandle) (*db.Summary, error)
	// Buffer buffers all records on the stream, records, summary and error will be received through call to Next
	// The Connection implementation should preserve/buffer streams automatically if needed when new
	// streams are created and the server doesn't support multiple streams. Use Buffer to force
	// buffering before calling Reset to get all records and the bookmark.
	Buffer(ctx context.Context, streamHandle StreamHandle) error
	// Bookmark returns the bookmark and optionally its database from last committed transaction or last finished auto-commit transaction.
	// The returned database is relevant for queries executed with the USE clause, since the returned database may be different from the session's database.
	// Note that if there is an ongoing auto-commit transaction (stream active) the bookmark
	// from that is not included, use Buffer or Consume to end the stream with a bookmark.
	// Empty string if no bookmark.
	Bookmark() string
	// ServerName returns the name of the remote server
	ServerName() string
	// ConnId returns the connection id as assigned by the server ("" if not available)
	ConnId() string
	// ServerVersion returns the server version on pattern Neo4j/1.2.3
	ServerVersion() string
	// IsAlive returns true if the connection is fully functional.
	// Implementation of this should be passive, no pinging or similar since it might be
	// called rather frequently.
	IsAlive() bool
	// HasFailed returns true if the connection has received a recoverable error (``FAILURE``).
	HasFailed() bool
	// Birthdate returns the point in time when this connection was established.
	Birthdate() time.Time
	// IdleDate returns the point in time since which the connection is idle
	IdleDate() time.Time
	// Reset resets connection to same state as directly after a connection.
	// Active streams will be discarded and the bookmark will be lost.
	Reset(ctx context.Context)
	// ForceReset behaves like Reset except it also resets connections in the
	// ready state (while Reset does not)
	ForceReset(ctx context.Context)
	// Close closes the database connection as well as any underlying connection.
	// The instance should not be used after being closed.
	Close(ctx context.Context)
	// GetRoutingTable gets the routing table for specified database name or the default database if
	// database equals DefaultDatabase. If the underlying connection does not support
	// multiple databases, DefaultDatabase should be used as database.
	// If user impersonation is used (impersonatedUser != "") and default database is used
	// the database name in the returned routing table will contain the actual name of the
	// configured default database for the impersonated user. If no impersonation is used
	// database name in routing table will be set to the name of the requested database.
	GetRoutingTable(ctx context.Context, context map[string]string, bookmarks []string, database, impersonatedUser string) (*RoutingTable, error)
	// SetBoltLogger sets Bolt message logger on already initialized connections
	SetBoltLogger(boltLogger log.BoltLogger)
	// ReAuth enqueues a `LOGOFF` and `LOGON` message if the passed credentials differ
	// or `ReAuthToken.ForceReAuth` is `true`.
	// If `ReAuthToken.ForceReAuth` is `true` the messages will be sent and their responses received.
	// If `ReAuthToken.FromSession` is `false`, the credentials changed, and the protocol version does not support
	// re-auth (bolt 5.1 and earlier) the connection will be closed.
	// If it's `true` (under otherwise same conditions) a `FeatureNotSupportedError` will be returned.
	ReAuth(context.Context, *ReAuthToken) error
	// Version returns the protocol version of the connection
	Version() db.ProtocolVersion
	// ResetAuth clears any authentication token held by this connection
	ResetAuth()
	// GetCurrentAuth returns the current authentication manager and token that this connection is authenticated with
	GetCurrentAuth() (auth.TokenManager, iauth.Token)
	// Telemetry sends telemetry information about the API usage to the server.
	Telemetry(api telemetry.API, onSuccess func())
	// SetPinHomeDatabaseCallback registers a callback to update the session's cached home database.
	// The callback is triggered on successful BEGIN or RUN responses containing a database name.
	SetPinHomeDatabaseCallback(callback func(ctx context.Context, database string))
	// IsSsrEnabled returns true if the connection supports Server-Side Routing.
	IsSsrEnabled() bool
}

type RoutingTable struct {
	TimeToLive   int
	DatabaseName string
	Routers      []string
	Readers      []string
	Writers      []string
}

// Marker for using the default database instance.
const DefaultDatabase = ""

// DatabaseSelector allows to select a database if the database server connection supports selecting which database instance on the server
// to connect to. Prior to Neo4j 4 there was only one database per server.
type DatabaseSelector interface {
	// SelectDatabase should be called immediately after Reset. Not allowed to call multiple times with different
	// databases without a reset in-between.
	SelectDatabase(database string)
	Database() string
}
