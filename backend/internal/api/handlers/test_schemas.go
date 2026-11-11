package handlers

// Schema for a single item
const itemSchema = `{
"type": "object",
"required": ["id", "name", "price"],
"properties": {
"id": {
"type": "integer",
"minimum": 1
},
"name": {
"type": "string",
"minLength": 1
},
"price": {
"type": "number",
"minimum": 0
}
}
}`

// Schema for an array of items
const itemListSchema = `{
"type": "array",
"items": ` + itemSchema + `
}`

// Schema for the GET /api/v1/ui-config response
const uiConfigSchema = `{
"type": "object",
"required": ["title", "auth_enabled", "oidc_enabled", "local_auth_enabled"],
"additionalProperties": false,
"properties": {
"title": {
"type": "string",
"minLength": 1
},
"auth_enabled": {
"type": "boolean"
},
"oidc_enabled": {
"type": "boolean"
},
"local_auth_enabled": {
"type": "boolean"
}
}
}`

// Schema for error responses
const errorSchema = `{
"type": "object",
"required": ["error"],
"properties": {
"error": {
"type": "string"
}
}
}`

// Schema for a user in an auth response. additionalProperties is false, so a
// password hash (or any other new field) in the JSON fails the check.
const userSchema = `{
"type": "object",
"required": ["id", "username", "role", "auth_provider", "disabled", "service_account", "created_at", "updated_at"],
"additionalProperties": false,
"properties": {
"id": {"type": "string", "minLength": 1},
"username": {"type": "string", "minLength": 1},
"display_name": {"type": "string"},
"email": {"type": "string"},
"role": {"type": "string", "enum": ["admin", "devops", "user"]},
"auth_provider": {"type": "string", "enum": ["local", "oidc"]},
"external_id": {"type": ["string", "null"]},
"disabled": {"type": "boolean"},
"service_account": {"type": "boolean"},
"created_at": {"type": "string"},
"updated_at": {"type": "string"}
}
}`

// Schema for the POST /api/v1/auth/login response.
const loginResponseSchema = `{
"type": "object",
"required": ["token", "user"],
"additionalProperties": false,
"properties": {
"token": {"type": "string", "minLength": 1},
"user": ` + userSchema + `
}
}`
