package computestorage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kikiChuang/hcsshim/internal/oc"
	"go.opencensus.io/trace"
)

// ExportLayer exports a container layer.
//
// `layerPath` is a path to a directory containing the layer to export.
//
// `exportFolderPath` is a pre-existing folder to export the layer to.
//
// `layerData` is the parent layer data.
//
// `options` are the export options applied to the exported layer.
func ExportLayer(ctx context.Context, layerPath, exportFolderPath string, layerData LayerData, options ExportLayerOptions) (err error) {
	title := "hcsshim.ExportLayer"
	ctx, span := trace.StartSpan(ctx, title)
	defer span.End()
	defer func() { oc.SetSpanStatus(span, err) }()
	span.AddAttributes(
		trace.StringAttribute("layerPath", layerPath),
		trace.StringAttribute("exportFolderPath", exportFolderPath),
	)

	ldbytes, err := json.Marshal(layerData)
	if err != nil {
		return err
	}

	obytes, err := json.Marshal(options)
	if err != nil {
		return err
	}

	err = hcsExportLayer(layerPath, exportFolderPath, string(ldbytes), string(obytes))
	if err != nil {
		return fmt.Errorf("failed to export layer: %s", err)
	}
	return nil
}
