package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gogo/protobuf/proto"

	datadogclient "github.com/DataDog/datadog-firehose-nozzle/internal/client/datadog"
	"github.com/DataDog/datadog-firehose-nozzle/internal/metric"
	. "github.com/DataDog/datadog-firehose-nozzle/test/helper"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("DatadogFirehoseNozzle", func() {
	var (
		fakeUAA        *FakeUAA
		fakeFirehose   *FakeFirehose
		fakeDatadogAPI *FakeDatadogAPI

		nozzleSession *gexec.Session
	)

	BeforeEach(func() {
		fakeUAA = NewFakeUAA("bearer", "123456789")
		fakeToken := fakeUAA.AuthToken()
		fakeFirehose = NewFakeFirehose(fakeToken)
		fakeDatadogAPI = NewFakeDatadogAPI()

		fakeUAA.Start()
		fakeFirehose.Start()
		fakeDatadogAPI.Start()

		os.Setenv("NOZZLE_FLUSHDURATIONSECONDS", "2")
		os.Setenv("NOZZLE_FLUSHMAXBYTES", "10240")
		os.Setenv("NOZZLE_UAAURL", fakeUAA.URL())
		os.Setenv("NOZZLE_DATADOGURL", fakeDatadogAPI.URL())
		os.Setenv("NOZZLE_TRAFFICCONTROLLERURL", strings.Replace(fakeFirehose.URL(), "http:", "ws:", 1))
		os.Setenv("NOZZLE_NUM_WORKERS", "1")
		os.Setenv("NOZZLE_ENVIRONMENT_NAME", "env_name")

		var err error
		nozzleCommand := exec.Command(pathToNozzleExecutable, "-config", "testdata/test-config.json")
		nozzleSession, err = gexec.Start(
			nozzleCommand,
			gexec.NewPrefixedWriter("[o][nozzle] ", GinkgoWriter),
			gexec.NewPrefixedWriter("[e][nozzle] ", GinkgoWriter),
		)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		fakeUAA.Close()
		fakeFirehose.Close()
		fakeDatadogAPI.Close()
		nozzleSession.Kill().Wait()
	})

	It("forwards metrics in a batch", func(done Done) {
		// Give time for the websocket connection to start
		time.Sleep(time.Second)
		fakeFirehose.AddEvent(events.Envelope{
			Origin:    proto.String("origin"),
			Timestamp: proto.Int64(1000000000),
			EventType: events.Envelope_ValueMetric.Enum(),
			ValueMetric: &events.ValueMetric{
				Name:  proto.String("metricName"),
				Value: proto.Float64(5),
				Unit:  proto.String("gauge"),
			},
			Deployment: proto.String("deployment-name-aaaaaaaaaaaaaaaaaaaa"),
			Job:        proto.String("doppler-partition-aaaaaaaaaaaaaaaaaaaa"),
			Index:      proto.String("1"),
		})

		fakeFirehose.AddEvent(events.Envelope{
			Origin:    proto.String("origin"),
			Timestamp: proto.Int64(2000000000),
			EventType: events.Envelope_ValueMetric.Enum(),
			ValueMetric: &events.ValueMetric{
				Name:  proto.String("metricName"),
				Value: proto.Float64(10),
				Unit:  proto.String("gauge"),
			},
			Deployment: proto.String("deployment-name-aaaaaaaaaaaaaaaaaaaa"),
			Job:        proto.String("gorouter-partition-aaaaaaaaaaaaaaaaaaaa"),
			Index:      proto.String("1"),
		})

		fakeFirehose.AddEvent(events.Envelope{
			Origin:    proto.String("origin"),
			Timestamp: proto.Int64(3000000000),
			EventType: events.Envelope_CounterEvent.Enum(),
			CounterEvent: &events.CounterEvent{
				Name:  proto.String("counterName"),
				Delta: proto.Uint64(3),
				Total: proto.Uint64(15),
			},
			Deployment: proto.String("deployment-name-aaaaaaaaaaaaaaaaaaaa"),
			Job:        proto.String("doppler-partition-aaaaaaaaaaaaaaaaaaaa"),
		})

		// eventually receive a batch from fake DD
		var messageBytes []byte
		Eventually(fakeDatadogAPI.ReceivedContents, "2s").Should(Receive(&messageBytes))

		// Break JSON blob into a list of blobs, one for each metric
		var payload datadogclient.Payload
		err := json.Unmarshal(Decompress(messageBytes), &payload)
		Expect(err).NotTo(HaveOccurred())

		for _, m := range payload.Series {
			Expect(m.Type).To(Equal("gauge"))

			if m.Metric == "cloudfoundry.nozzle.origin.metricName" || m.Metric == "cloudfoundry.nozzle.metricName" {
				Expect(m.Tags).To(HaveLen(9))
				Expect(m.Tags[0]).To(Equal("deployment:deployment-name"))
				if m.Tags[5] == "job:doppler" {
					Expect(m.Points).To(Equal([]metric.Point{
						{Timestamp: 1, Value: 5.0},
					}))
				} else if m.Tags[5] == "job:gorouter" {
					Expect(m.Points).To(Equal([]metric.Point{
						{Timestamp: 2, Value: 10.0},
					}))
				} else {
					panic("Unknown tag")
				}
			} else if m.Metric == "cloudfoundry.nozzle.origin.counterName" || m.Metric == "cloudfoundry.nozzle.counterName" {
				Expect(m.Tags).To(HaveLen(8))
				Expect(m.Tags[0]).To(Equal("deployment:deployment-name"))
				Expect(m.Tags[1]).To(Equal("deployment:deployment-name-aaaaaaaaaaaaaaaaaaaa"))
				Expect(m.Tags[2]).To(Equal("deployment:deployment-name_env_name"))
				Expect(m.Tags[3]).To(Equal("env:env_name"))
				Expect(m.Tags[4]).To(Equal("job:doppler"))
				Expect(m.Tags[5]).To(Equal("job:doppler-partition-aaaaaaaaaaaaaaaaaaaa"))
				Expect(m.Tags[6]).To(Equal("name:origin"))
				Expect(m.Tags[7]).To(Equal("origin:origin"))

				Expect(m.Points).To(Equal([]metric.Point{
					{Timestamp: 3, Value: 15.0},
				}))
			} else if m.Metric == "cloudfoundry.nozzle.totalMessagesReceived" {
				Expect(m.Tags).To(HaveLen(2))
				Expect(m.Tags[0]).To(HavePrefix("deployment:"))
				Expect(m.Tags[1]).To(HavePrefix("ip:"))

				Expect(m.Points).To(HaveLen(1))
				Expect(m.Points[0].Value).To(Equal(3.0))
			} else if m.Metric == "cloudfoundry.nozzle.totalMetricsSent" {
				Expect(m.Tags).To(HaveLen(2))
				Expect(m.Tags[0]).To(HavePrefix("deployment:"))
				Expect(m.Tags[1]).To(HavePrefix("ip:"))

				Expect(m.Points).To(HaveLen(1))
				Expect(m.Points[0].Value).To(Equal(0.0))
			} else if m.Metric == "cloudfoundry.nozzle.slowConsumerAlert" {

			} else {
				panic("Unknown metric " + m.Metric)
			}
		}

		close(done)
	}, 4.0)
})
