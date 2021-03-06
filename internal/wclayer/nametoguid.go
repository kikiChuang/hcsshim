package wclayer

import (
	"context"

	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/kikiChuang/hcsshim/internal/hcserror"
	"github.com/kikiChuang/hcsshim/internal/oc"
	"go.opencensus.io/trace"
)

// NameToGuid converts the given string into a GUID using the algorithm in the
// Host Compute Service, ensuring GUIDs generated with the same string are common
// across all clients.
func NameToGuid(ctx context.Context, name string) (_ guid.GUID, err error) {
	title := "hcsshim::NameToGuid"
	ctx, span := trace.StartSpan(ctx, title)
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(trace.StringAttribute("name", name))

	var id guid.GUID
	err = nameToGuid(name, &id)
	if err != nil {
		return guid.GUID{}, hcserror.New(err, title+" - failed", "")
	}
	span.AddAttributes(trace.StringAttribute("guid", id.String()))
	return id, nil
}
