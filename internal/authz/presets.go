// SPDX-License-Identifier: AGPL-3.0-only

package authz

// Preset role names. The preset set is a proposed starting point (U02 asks
// for preset roles plus customization); custom roles cover other needs.
const (
	RoleViewer        = "viewer"
	RoleOperator      = "operator"
	RoleDatabaseAdmin = "database_admin"
	RoleAdministrator = "administrator"
	// RoleManagerSystem is the system role of the manager's own subject.
	RoleManagerSystem = "manager_system"
)

// PresetRoles returns the immutable preset roles.
func PresetRoles() []Role {
	return []Role{
		{
			Name:   RoleViewer,
			Preset: true,
			Permissions: []Permission{
				{Type: ResourceSwarm, Action: ActionRead},
				{Type: ResourceHost, Action: ActionRead},
				{Type: ResourceCluster, Action: ActionRead},
				{Type: ResourceDatabase, Action: ActionRead},
				{Type: ResourceBackup, Action: ActionRead},
				{Type: ResourceDashboard, Action: ActionRead},
				{Type: ResourceImageBuild, Action: ActionRead},
				{Type: ResourceRegistry, Action: ActionRead},
				{Type: ResourceService, Action: ActionRead},
			},
		},
		{
			Name:   RoleOperator,
			Preset: true,
			Permissions: []Permission{
				{Type: ResourceSwarm, Action: ActionRead},
				{Type: ResourceHost, Action: ActionRead},
				{Type: ResourceHost, Action: ActionOperate},
				{Type: ResourceCluster, Action: ActionRead},
				{Type: ResourceCluster, Action: ActionOperate},
				{Type: ResourceDatabase, Action: ActionRead},
				{Type: ResourceBackup, Action: ActionRead},
				{Type: ResourceBackup, Action: ActionBackup},
				{Type: ResourceDashboard, Action: ActionRead},
				{Type: ResourceImageBuild, Action: ActionRead},
				{Type: ResourceImageBuild, Action: ActionCreate},
				{Type: ResourceRegistry, Action: ActionRead},
				{Type: ResourceService, Action: ActionRead},
			},
		},
		{
			Name:   RoleDatabaseAdmin,
			Preset: true,
			Permissions: []Permission{
				{Type: ResourceCluster, Action: ActionRead},
				{Type: ResourceDatabase, Action: ActionAny},
				{Type: ResourceDatabaseUser, Action: ActionAny},
				{Type: ResourceBackup, Action: ActionRead},
				{Type: ResourceBackup, Action: ActionBackup},
				{Type: ResourceDashboard, Action: ActionRead},
			},
		},
		{
			Name:        RoleAdministrator,
			Preset:      true,
			Permissions: []Permission{{Type: ResourceAny, Action: ActionAny}},
		},
		{
			Name:        RoleManagerSystem,
			Preset:      true,
			System:      true,
			Permissions: []Permission{{Type: ResourceAny, Action: ActionAny}},
		},
	}
}
