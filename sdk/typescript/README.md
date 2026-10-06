# sandboxlab@1.0.0

A TypeScript SDK client for the sandboxlab.example.com API.

## Usage

First, install the SDK from npm.

```bash
npm install sandboxlab --save
```

Next, try it out.


```ts
import {
  Configuration,
  MetaApi,
} from 'sandboxlab';
import type { DescribeRequest } from 'sandboxlab';

async function example() {
  console.log("🚀 Testing sandboxlab SDK...");
  const api = new MetaApi();

  try {
    const data = await api.describe();
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```


## Documentation

### API Endpoints

All URIs are relative to *https://sandboxlab.example.com/sandboxlab*

| Class | Method | HTTP request | Description
| ----- | ------ | ------------ | -------------
*MetaApi* | [**describe**](docs/MetaApi.md#describe) | **GET** /api/v1/describe | This API\&#39;s own contract
*MetaApi* | [**getConfig**](docs/MetaApi.md#getconfig) | **GET** /api/v1/config | The deployment\&#39;s shape
*MetaApi* | [**health**](docs/MetaApi.md#health) | **GET** /healthz | Liveness
*MetaApi* | [**ready**](docs/MetaApi.md#ready) | **GET** /readyz | Readiness
*SandboxesApi* | [**addCatalogEntry**](docs/SandboxesApi.md#addcatalogentry) | **POST** /api/v1/catalog | Add a template to the running catalog
*SandboxesApi* | [**createSandbox**](docs/SandboxesApi.md#createsandboxoperation) | **POST** /api/v1/sandboxes | Create one
*SandboxesApi* | [**deleteCatalogEntry**](docs/SandboxesApi.md#deletecatalogentry) | **DELETE** /api/v1/catalog/{id} | Remove a template from the running catalog
*SandboxesApi* | [**deleteSandbox**](docs/SandboxesApi.md#deletesandbox) | **DELETE** /api/v1/sandboxes/{id} | Delete one
*SandboxesApi* | [**execInSandbox**](docs/SandboxesApi.md#execinsandbox) | **POST** /api/v1/sandboxes/{id}/exec | Run a command in a sandbox and wait for it
*SandboxesApi* | [**getCatalogEntry**](docs/SandboxesApi.md#getcatalogentry) | **GET** /api/v1/catalog/{id} | One template
*SandboxesApi* | [**getOverview**](docs/SandboxesApi.md#getoverview) | **GET** /api/v1/overview | Counts of sandboxes by state and template
*SandboxesApi* | [**getSandbox**](docs/SandboxesApi.md#getsandbox) | **GET** /api/v1/sandboxes/{id} | One sandbox
*SandboxesApi* | [**getSandboxEvents**](docs/SandboxesApi.md#getsandboxevents) | **GET** /api/v1/sandboxes/{id}/events | Recent cluster events about a sandbox
*SandboxesApi* | [**getSandboxKey**](docs/SandboxesApi.md#getsandboxkey) | **GET** /api/v1/sandboxes/{id}/key | A sandbox\&#39;s own API key
*SandboxesApi* | [**getSandboxLogs**](docs/SandboxesApi.md#getsandboxlogs) | **GET** /api/v1/sandboxes/{id}/logs | The tail of a sandbox\&#39;s output
*SandboxesApi* | [**getSandboxUsage**](docs/SandboxesApi.md#getsandboxusage) | **GET** /api/v1/sandboxes/{id}/usage | What a sandbox is using right now
*SandboxesApi* | [**listCatalog**](docs/SandboxesApi.md#listcatalog) | **GET** /api/v1/catalog | The templates a sandbox can be created from
*SandboxesApi* | [**listSandboxes**](docs/SandboxesApi.md#listsandboxes) | **GET** /api/v1/sandboxes | Every sandbox in the deployment
*SandboxesApi* | [**proxyToSandbox**](docs/SandboxesApi.md#proxytosandbox) | **GET** /sandbox/{id}/{port}/ | Proxy to a sandbox\&#39;s own port
*SandboxesApi* | [**readSandboxFile**](docs/SandboxesApi.md#readsandboxfile) | **GET** /api/v1/sandboxes/{id}/files | Read a file out of a sandbox
*SandboxesApi* | [**renewSandbox**](docs/SandboxesApi.md#renewsandboxoperation) | **POST** /api/v1/sandboxes/{id}/renew | Reset a sandbox\&#39;s lifetime, measured from now
*SandboxesApi* | [**rotateSandboxKey**](docs/SandboxesApi.md#rotatesandboxkey) | **POST** /api/v1/sandboxes/{id}/key/rotate | Replace a sandbox\&#39;s key
*SandboxesApi* | [**writeSandboxFile**](docs/SandboxesApi.md#writesandboxfile) | **PUT** /api/v1/sandboxes/{id}/files | Write a file into a sandbox


### Models

- [AddTemplateRequest](docs/AddTemplateRequest.md)
- [AuthDescription](docs/AuthDescription.md)
- [Config](docs/Config.md)
- [CreateSandboxRequest](docs/CreateSandboxRequest.md)
- [DeleteSandbox200Response](docs/DeleteSandbox200Response.md)
- [Deleted](docs/Deleted.md)
- [Describe](docs/Describe.md)
- [Endpoint](docs/Endpoint.md)
- [EndpointDescription](docs/EndpointDescription.md)
- [Event](docs/Event.md)
- [ExecRequest](docs/ExecRequest.md)
- [ExecResult](docs/ExecResult.md)
- [FileContent](docs/FileContent.md)
- [FileInfo](docs/FileInfo.md)
- [GetSandboxEvents200Response](docs/GetSandboxEvents200Response.md)
- [GetSandboxLogs200Response](docs/GetSandboxLogs200Response.md)
- [ModelError](docs/ModelError.md)
- [Overview](docs/Overview.md)
- [Port](docs/Port.md)
- [RenewSandboxRequest](docs/RenewSandboxRequest.md)
- [Resources](docs/Resources.md)
- [Sandbox](docs/Sandbox.md)
- [SandboxKey](docs/SandboxKey.md)
- [SandboxList](docs/SandboxList.md)
- [SandboxState](docs/SandboxState.md)
- [Status](docs/Status.md)
- [Template](docs/Template.md)
- [TemplateList](docs/TemplateList.md)
- [Usage](docs/Usage.md)
- [WriteFileRequest](docs/WriteFileRequest.md)

### Authorization


Authentication schemes defined for the API:
<a id="ApiKey"></a>
#### ApiKey


- **Type**: API key
- **API key parameter name**: `X-Sandbox-Key`
- **Location**: HTTP header
<a id="BearerAuth"></a>
#### BearerAuth


- **Type**: HTTP Bearer Token authentication
<a id="ApiKeyQuery"></a>
#### ApiKeyQuery


- **Type**: API key
- **API key parameter name**: `key`
- **Location**: URL query string

## About

This TypeScript SDK client supports the [Fetch API](https://fetch.spec.whatwg.org/)
and is automatically generated by the
[OpenAPI Generator](https://openapi-generator.tech) project:

- API version: `1.0`
- Package version: `1.0.0`
- Generator version: `7.17.0`
- Build package: `org.openapitools.codegen.languages.TypeScriptFetchClientCodegen`

The generated npm module supports the following:

- Environments
  * Node.js
  * Webpack
  * Browserify
- Language levels
  * ES5 - you must have a Promises/A+ library installed
  * ES6
- Module systems
  * CommonJS
  * ES6 module system


## Development

### Building

To build the TypeScript source code, you need to have Node.js and npm installed.
After cloning the repository, navigate to the project directory and run:

```bash
npm install
npm run build
```

### Publishing

Once you've built the package, you can publish it to npm:

```bash
npm publish
```

## License

[]()
