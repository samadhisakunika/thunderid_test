/*
Copyright 2026.

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

package v1alpha1

// InstanceRefLabel is the metadata.labels key a Resource (any spec.resource_type — application,
// flow, theme, organization_unit, role, user_type, user, group,
// translation, etc..) uses to reference its owning ThunderIDInstance, instead of a spec
// field. Keeping it out of spec means adding a new resource_type never requires touching the
// operator's schema.
//
// An Environment uses the same label to name the ThunderIDInstance it belongs to: see
// findEnvironment in env_secret.go. Unlike a Resource, the label is optional on an Environment —
// its own reconcile (syncing each section's Env into its Secret) doesn't need an owning instance
// at all, the label only controls whether some ThunderIDInstance auto-discovers it.
const InstanceRefLabel = "thunderid.io/instance"
