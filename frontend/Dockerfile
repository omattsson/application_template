# Build stage
FROM node:20-alpine as build

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install dependencies
RUN npm install

# Copy source code
COPY . .

# ARG for build/dev mode
ARG NODE_ENV=production

# If production, build the app, otherwise use development mode
RUN if [ "$NODE_ENV" = "production" ]; then \
        npm run build; \
    fi

# For production, use nginx to serve the built app
FROM nginx:alpine as production
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
CMD ["nginx", "-g", "daemon off;"]

# For development, use the node image to run the dev server
FROM build as development
EXPOSE 3000
CMD ["npm", "run", "dev", "--", "--host", "0.0.0.0"]
