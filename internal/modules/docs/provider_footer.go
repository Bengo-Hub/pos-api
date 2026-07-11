package docs

// Platform-owner (Codevertex) advertisement lines rendered at the bottom of every generated
// report/document. The values mirror the codevertex platform-owner tenant's public auth-api
// record; they are static here because the report renderer is a pure function with no tenant-cache
// access, and the provider's own contact details change rarely. Keep this block identical across
// the sibling docs engines (pos-api, inventory-api, treasury-api docs/reports) so every generated
// document advertises the SaaS provider uniformly.
const (
	providerFooterLead    = "Developed & maintained by Codevertex Africa Limited"
	providerFooterContact = "www.codevertexitsolutions.com  ·  info@codevertexitsolutions.com  ·  +254 742 201 368"
)
