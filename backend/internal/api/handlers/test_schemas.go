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
"required": ["title", "auth_enabled"],
"additionalProperties": false,
"properties": {
"title": {
"type": "string",
"minLength": 1
},
"auth_enabled": {
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
