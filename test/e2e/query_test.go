package e2e_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/common/model"
	"github.com/thanos-io/thanos/pkg/promclient"
	"github.com/thanos-io/thanos/pkg/runutil"
	"github.com/thanos-io/thanos/pkg/testutil"
)

type testConfig struct {
	name  string
	suite *spinupSuite
}

var (
	firstPromPort = promHTTPPort(1)

	queryStaticFlagsSuite = newSpinupSuite().
				Add(scraper(1, defaultPromConfig("prom-"+firstPromPort, 0))).
				Add(scraper(2, defaultPromConfig("prom-ha", 0))).
				Add(scraper(3, defaultPromConfig("prom-ha", 1))).
				Add(querierWithStoreFlags(1, "replica", sidecarGRPC(1), sidecarGRPC(2), sidecarGRPC(3), remoteWriteReceiveGRPC(1))).
				Add(querierWithStoreFlags(2, "replica", sidecarGRPC(1), sidecarGRPC(2), sidecarGRPC(3), remoteWriteReceiveGRPC(1))).
				Add(receiver(1, defaultPromRemoteWriteConfig(nodeExporterHTTP(1), remoteWriteEndpoint(1)), 1))

	queryFileSDSuite = newSpinupSuite().
				Add(scraper(1, defaultPromConfig("prom-"+firstPromPort, 0))).
				Add(scraper(2, defaultPromConfig("prom-ha", 0))).
				Add(scraper(3, defaultPromConfig("prom-ha", 1))).
				Add(querierWithFileSD(1, "replica", sidecarGRPC(1), sidecarGRPC(2), sidecarGRPC(3), remoteWriteReceiveGRPC(1))).
				Add(querierWithFileSD(2, "replica", sidecarGRPC(1), sidecarGRPC(2), sidecarGRPC(3), remoteWriteReceiveGRPC(1))).
				Add(receiver(1, defaultPromRemoteWriteConfig(nodeExporterHTTP(1), remoteWriteEndpoint(1)), 1))
)

func TestQuery(t *testing.T) {
	for _, tt := range []testConfig{
		{
			"staticFlag",
			queryStaticFlagsSuite,
		},
		{
			"fileSD",
			queryFileSDSuite,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testQuerySimple(t, tt)
		})
	}
}

// testQuerySimple runs a setup of Prometheus servers, sidecars, and query nodes and verifies that
// queries return data merged from all Prometheus servers. Additionally it verifies if deduplication works for query.
func testQuerySimple(t *testing.T, conf testConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)

	exit, err := conf.suite.Exec(t, ctx, conf.name)
	if err != nil {
		t.Errorf("spinup failed: %v", err)
		cancel()
		return
	}

	defer func() {
		cancel()
		<-exit
	}()

	var res model.Vector

	w := log.NewSyncWriter(os.Stderr)
	l := log.NewLogfmtLogger(w)
	l = log.With(l, "conf-name", conf.name)

	// Try query without deduplication.
	testutil.Ok(t, runutil.RetryWithLog(l, time.Second, ctx.Done(), func() error {
		select {
		case <-exit:
			cancel()
			return errors.Errorf("exiting test, possibly due to timeout")
		default:
		}

		var (
			err      error
			warnings []string
		)
		res, warnings, err = promclient.QueryInstant(ctx, nil, urlParse(t, "http://"+queryHTTP(1)), "up", time.Now(), promclient.QueryOptions{
			Deduplicate: false,
		})
		if err != nil {
			return err
		}

		if len(warnings) > 0 {
			// we don't expect warnings.
			return errors.Errorf("unexpected warnings %s", warnings)
		}

		expectedRes := 4
		if len(res) != expectedRes {
			return errors.Errorf("unexpected result size %d, expected %d", len(res), expectedRes)
		}
		return nil
	}))

	// In our model result are always sorted.
	testutil.Equals(t, model.Metric{
		"__name__":   "up",
		"instance":   model.LabelValue(promHTTP(1)),
		"job":        "prometheus",
		"prometheus": model.LabelValue("prom-" + promHTTPPort(1)),
		"replica":    model.LabelValue("0"),
	}, res[0].Metric)
	testutil.Equals(t, model.Metric{
		"__name__":   "up",
		"instance":   model.LabelValue(promHTTP(1)),
		"job":        "prometheus",
		"prometheus": "prom-ha",
		"replica":    model.LabelValue("0"),
	}, res[1].Metric)
	testutil.Equals(t, model.Metric{
		"__name__":   "up",
		"instance":   model.LabelValue(promHTTP(1)),
		"job":        "prometheus",
		"prometheus": "prom-ha",
		"replica":    model.LabelValue("1"),
	}, res[2].Metric)

	testutil.Equals(t, model.Metric{
		"__name__": "up",
		"instance": model.LabelValue(nodeExporterHTTP(1)),
		"job":      "node",
		"receive":  "true",
		"replica":  model.LabelValue("1"),
	}, res[3].Metric)

	// Try query with deduplication.
	testutil.Ok(t, runutil.Retry(time.Second, ctx.Done(), func() error {
		select {
		case <-exit:
			cancel()
			return nil
		default:
		}

		var (
			err      error
			warnings []string
		)
		res, warnings, err = promclient.QueryInstant(ctx, nil, urlParse(t, "http://"+queryHTTP(1)), "up", time.Now(), promclient.QueryOptions{
			Deduplicate: true,
		})
		if err != nil {
			return err
		}

		if len(warnings) > 0 {
			// we don't expect warnings.
			return errors.Errorf("unexpected warnings %s", warnings)
		}

		expectedRes := 3
		if len(res) != expectedRes {
			return errors.Errorf("unexpected result size %d, expected %d", len(res), expectedRes)
		}

		return nil
	}))

	testutil.Equals(t, model.Metric{
		"__name__":   "up",
		"instance":   model.LabelValue(promHTTP(1)),
		"job":        "prometheus",
		"prometheus": model.LabelValue("prom-" + promHTTPPort(1)),
	}, res[0].Metric)
	testutil.Equals(t, model.Metric{
		"__name__":   "up",
		"instance":   model.LabelValue(promHTTP(1)),
		"job":        "prometheus",
		"prometheus": "prom-ha",
	}, res[1].Metric)
	testutil.Equals(t, model.Metric{
		"__name__": "up",
		"instance": model.LabelValue(nodeExporterHTTP(1)),
		"job":      "node",
		"receive":  "true",
	}, res[2].Metric)
}

func urlParse(t *testing.T, addr string) *url.URL {
	u, err := url.Parse(addr)
	testutil.Ok(t, err)

	return u
}

func defaultPromConfig(name string, replicas int) string {
	return fmt.Sprintf(`
global:
  external_labels:
    prometheus: %s
    replica: %v
scrape_configs:
- job_name: prometheus
  scrape_interval: 1s
  static_configs:
  - targets:
    - "localhost:%s"
`, name, replicas, firstPromPort)
}

func defaultPromRemoteWriteConfig(nodeExporterHTTP, remoteWriteEndpoint string) string {
	return fmt.Sprintf(`
scrape_configs:
- job_name: 'node'
  static_configs:
  - targets: ['%s']
remote_write:
- url: "%s"
`, nodeExporterHTTP, remoteWriteEndpoint)
}
