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
