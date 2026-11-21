/**
 * The filter options of the audit log page. These are copies of the backend
 * lists `KnownAuditActions` and `KnownAuditEntityTypes` in
 * `backend/internal/api/middleware/audit.go`: keep them in sync. An entry with
 * another value is still listed on the page; it is only not offered as a filter.
 */

/** The actions that new audit entries can have. */
export const AUDIT_ACTIONS: readonly string[] = [
  'create',
  'update',
  'delete',
  'disable',
  'enable',
  'change_role',
  'change_password',
  'password_reset',
];

/** The entity types that new audit entries can have. */
export const AUDIT_ENTITY_TYPES: readonly string[] = ['item', 'user', 'api_key'];
