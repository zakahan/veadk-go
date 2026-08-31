// Copyright (c) 2025 Beijing Volcano Engine Technology Co., Ltd. and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import "context"

// APIKeyProvider resolves a model API key at request time.
type APIKeyProvider interface {
	APIKey(ctx context.Context) (string, error)
}

// APIKeyInvalidator allows a model client to discard a cached key after an
// authentication failure. Providers that cache keys should implement both
// APIKeyProvider and APIKeyInvalidator to enable one refresh retry on 401/403.
type APIKeyInvalidator interface {
	Invalidate()
}
