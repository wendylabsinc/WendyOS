package data

import "github.com/wendylabsinc/wendy/go/internal/shared/datadoc"

// The document kinds this agent stores and runs itself. A collection is a
// campaign and keeps its own storage.
const (
	KindInference = "inference"
	KindWatch     = "watch"
)

// documentKinds are the data document kinds this build understands. A
// collection is a campaign: its decoder is ParseCampaign, so collections parse,
// validate and hash exactly as campaigns always have.
var documentKinds = datadoc.NewRegistry(
	datadoc.Kind{
		Name:     datadoc.DefaultKind,
		Versions: []int{CampaignVersion},
		Decode: func(_ datadoc.Header, contents []byte) (any, string, []string, error) {
			campaign, err := ParseCampaign(contents)
			if err != nil {
				return nil, "", nil, err
			}
			return campaign, campaign.Revision, nil, nil
		},
	},
	datadoc.Kind{Name: KindInference, Versions: []int{InferenceDocumentVersion}, Ownable: true, Decode: decodeInferenceDocument},
	datadoc.Kind{Name: KindWatch, Versions: []int{WatchVersion}, Ownable: true, Decode: decodeWatch},
)

// ParseDocument parses any data document this build knows. A file without
// kind is a collection.
func ParseDocument(contents []byte) (datadoc.Document, error) {
	return documentKinds.Parse(contents)
}

// DocumentHeader checks only a document's header, for callers that sort files
// by kind without validating them.
func DocumentHeader(contents []byte) (datadoc.Header, error) {
	return documentKinds.ParseHeader(contents)
}
