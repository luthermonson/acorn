package log

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/acorn-io/acorn/integration/helper"
	apiv1 "github.com/acorn-io/acorn/pkg/apis/api.acorn.io/v1"
	"github.com/acorn-io/acorn/pkg/build"
	"github.com/stretchr/testify/assert"
)

const sampleLog = `line 1-1
line 1-2
line 1-3
line 1-4
line 2-1
line 2-2
line 2-3
line 2-4`

func TestLog(t *testing.T) {
	c, ns := helper.ClientAndNamespace(t)

	image, err := build.Build(helper.GetCTX(t), "./testdata/Acornfile", &build.Options{
		Client: helper.BuilderClient(t, ns.Name),
		Cwd:    "./testdata",
	})
	if err != nil {
		t.Fatal(err)
	}

	app, err := c.AppRun(context.Background(), image.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app = helper.WaitForObject(t, c.GetClient().Watch, &apiv1.AppList{}, app, func(app *apiv1.App) bool {
		return app.Status.ContainerStatus["cont1-1"].Ready == 1 &&
			app.Status.ContainerStatus["cont1-2"].Ready == 1
	})

	output, err := c.AppLog(ctx, app.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for msg := range output {
		if msg.Error != "" {
			if len(lines) < 8 && !strings.Contains(msg.Error, "context canceled") {
				t.Fatal(msg.Error)
			}
			continue
		}
		lines = append(lines, msg.Line)
		if len(lines) >= 8 {
			cancel()
		}
	}

	sort.Strings(lines)
	assert.Equal(t, sampleLog, strings.Join(lines, "\n"))
}
