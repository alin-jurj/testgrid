/*
Copyright 2022 The TestGrid Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tabulator

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"sync"
	"time"

	"bitbucket.org/creachadair/stringset"
	"github.com/GoogleCloudPlatform/testgrid/config"
	"github.com/GoogleCloudPlatform/testgrid/config/snapshot"
	"github.com/GoogleCloudPlatform/testgrid/pkg/updater"
	"github.com/GoogleCloudPlatform/testgrid/util/gcs"
	"github.com/GoogleCloudPlatform/testgrid/util/metrics"
	"github.com/sirupsen/logrus"
)

const componentName = "tabulator"

// Metrics holds metrics relevant to this controller.
type Metrics struct {
	UpdateState  metrics.Cyclic
	DelaySeconds metrics.Duration
}

// CreateMetrics creates metrics for this controller
func CreateMetrics(factory metrics.Factory) *Metrics {
	return &Metrics{
		UpdateState:  factory.NewCyclic(componentName),
		DelaySeconds: factory.NewDuration("delay", "Seconds tabulator is behind schedule", "component"),
	}
}

// Fixer should adjust the dashboard queue until the context expires.
type Fixer func(context.Context, *config.DashboardQueue) error

// For each dashboard/tab in the config, copy the testgroup state into the tab state.
func Update(ctx context.Context, client gcs.ConditionalClient, mets *Metrics, configPath gcs.Path, concurrency int, gridPathPrefix, tabsPathPrefix string, confirm bool, freq time.Duration, fix Fixer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if concurrency < 1 {
		return fmt.Errorf("concurrency must be positive, got: %d", concurrency)
	}
	log := logrus.WithField("config", configPath)

	var q config.DashboardQueue
	if fix != nil {
		go func() {
			fix(ctx, &q)
		}()
	}

	onUpdate := func(cfg *snapshot.Config) error {
		// reinitialize all
		q.Init(log, cfg.AllDashboards(), time.Now())
		return nil
	}

	log.Debug("Observing config...")
	cfg, err := snapshot.Observe(ctx, log, client, configPath, onUpdate)
	if err != nil {
		return fmt.Errorf("error while observing config %q: %w", configPath.String(), err)
	}

	q.Init(log, cfg.AllDashboards(), time.Now())

	// Set up logging thread
	go func(ctx context.Context) {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			depth, next, when := q.Status()
			log := log.WithField("depth", depth)
			if next != nil {
				log = log.WithField("next", &next)
			}
			delay := time.Since(when)
			if delay < 0 {
				delay = 0
				log = log.WithField("sleep", -delay)
			}
			mets.DelaySeconds.Set(delay, componentName)
			log.Debug("Calculated metrics")

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}(ctx)

	// Set up threads
	var active stringset.Set
	var waiting stringset.Set
	var lock sync.Mutex

	dashboardNames := make(chan string)

	update := func(log *logrus.Entry, dashName string) error {
		_, tabs := cfg.DashboardTestGroups(dashName)

		for tabName, testGroup := range tabs {
			fromPath, err := updater.TestGroupPath(configPath, gridPathPrefix, testGroup.Name)
			if err != nil {
				return fmt.Errorf("can't make tg path %q: %w", testGroup.Name, err)
			}
			toPath, err := tabStatePath(configPath, tabsPathPrefix, dashName, tabName)
			if err != nil {
				return fmt.Errorf("can't make dashtab path %s/%s: %w", dashName, tabName, err)
			}
			log.WithFields(logrus.Fields{
				"from": fromPath.String(),
				"to":   toPath.String(),
			}).Info("Copying state")
			if confirm {
				_, err = client.Copy(ctx, *fromPath, *toPath)
				if err != nil {
					return fmt.Errorf("can't copy from %q to %q: %w", fromPath.String(), toPath.String(), err)
				}
			}
		}
		return nil
	}

	// Run threads continuously
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			for dashName := range dashboardNames {
				lock.Lock()
				start := active.Add(dashName)
				if !start {
					waiting.Add(dashName)
				}
				lock.Unlock()
				if !start {
					continue
				}

				log := log.WithField("dashboard", dashName)
				finish := mets.UpdateState.Start()

				if err := update(log, dashName); err != nil {
					finish.Fail()
					q.Fix(dashName, time.Now().Add(freq/2), false)
					log.WithError(err).Error("Failed to generate tab state")
				} else {
					finish.Success()
					log.Info("Built tab state")
				}

				lock.Lock()
				active.Discard(dashName)
				restart := waiting.Discard(dashName)
				lock.Unlock()
				if restart {
					q.Fix(dashName, time.Now(), false)
				}
			}
		}()
	}
	defer wg.Wait()
	defer close(dashboardNames)

	return q.Send(ctx, dashboardNames, freq)
}

func tabStatePath(g gcs.Path, tabPrefix, dashboardName, tabName string) (*gcs.Path, error) {
	name := path.Join(tabPrefix, dashboardName, tabName)
	u, err := url.Parse(name)
	if err != nil {
		return nil, fmt.Errorf("invalid url %s: %w", name, err)
	}
	np, err := g.ResolveReference(u)
	if err != nil {
		return nil, fmt.Errorf("resolve reference: %w", err)
	}
	if err == nil && np.Bucket() != g.Bucket() {
		return nil, fmt.Errorf("tabState %s should not change bucket", name)
	}
	return np, nil
}