/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file include shared code for storage clients.

package store

import (
	"context"
	"time"
)

// -- Admin interfaces --

// BatchClientAdmin specifies administrative interface functions.
type BatchClientAdmin interface {

	// GetContext returns a derived context for a call.
	// If no time limit is set, the context will be set with a default time limit.
	GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc)

	// Close closes the client.
	Close() error
}
