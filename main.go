package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"strconv"
	"strings"
	"text/template"
	"time"

	MQTT "github.com/eclipse/paho.mqtt.golang"
	"github.com/fsnotify/fsnotify"
	"github.com/hnesland/telldusmq/tellduscore"
	"github.com/spf13/viper"
)

// TelldusEvent describes telldus events ..
type TelldusEvent struct {
	Class    string
	Protocol string
	Model    string
	Code     string
	House    string
	Unit     string
	Group    string
	Method   string
	Id       string
	Temp     string
	Humidity string
	Value    string
	DataType string
}

// TellstickMQTTBrokerEvent describes incoming events on MQTT
type TellstickMQTTBrokerEvent struct {
	Protocol string `json:"protocol"`
	DeviceID int    `json:"device_id"`
	House    uint   `json:"house"`
	Unit     int    `json:"unit"`
	Method   string `json:"method"`
	Level    int    `json:"level"`
}

var mqttClient MQTT.Client
var telldusSocket net.Conn

// readTelldusRawDeviceEvents reads from Telldus unix socket and parses the raw event strings in rawTelldusEvent()
func readTelldusRawDeviceEvents(errCh chan error) {
	buf := make([]byte, 1024)
	for {
		if telldusSocket != nil {
			n, err := telldusSocket.Read(buf[:])
			if err != nil {
				log.Printf("read error: %q", err)
				errCh <- err
				return
			}

			telldusEventString := string(buf[0:n])
			if strings.Contains(telldusEventString, "TDRawDeviceEvent") {
				classBegins := strings.Index(telldusEventString, "class")
				str := buf[classBegins : n-3]
				rawTelldusEvent(string(str))
			}
		} else {
			errCh <- errors.New("socket not available")
			return
		}
	}
}

// sendTelldusDeviceEvent transmits a telldus-event to the client unix socket.
func sendTelldusDeviceEvent(message string) string {
	tellstickClientUnixSocket := viper.GetString("Tellstick.UnixSocketClient")
	if tellstickClientUnixSocket == "" {
		tellstickClientUnixSocket = "/tmp/TelldusClient"
	}

	clientSocket, err := net.Dial("unix", tellstickClientUnixSocket)
	if err != nil {
		return ""
	}

	clientSocket.Write([]byte(message))
	buf := make([]byte, 1024)
	n, err := clientSocket.Read(buf[:])
	telldusResult := string(buf[0:n])

	return telldusResult
}

// setupTelldus initializes the event unix socket from telldus
func setupTelldus() {
	errCh := make(chan error)

	tellstickEventsUnixSocket := viper.GetString("Tellstick.UnixSocketEvents")
	if tellstickEventsUnixSocket == "" {
		tellstickEventsUnixSocket = "/tmp/TelldusEvents"
	}

	for {
		log.Printf("Connecting to Telldusd Events Socket\n")
		var err error
		telldusSocket, err = net.Dial("unix", tellstickEventsUnixSocket)

		go readTelldusRawDeviceEvents(errCh)
		err = <-errCh

		if err != nil {
			log.Printf("Telldusd connection error: %q: Please verify that '%s' is readable. Retry in 5 sec ..\n", err, tellstickEventsUnixSocket)
		}

		time.Sleep(5 * time.Second)
	}
}

func rawTelldusEvent(str string) {
	data := strings.Split(str, ";")
	event := &TelldusEvent{
		Id:       "0",
		House:    "0",
		Unit:     "0",
		Code:     "0",
		Group:    "0",
		Temp:     "0",
		Humidity: "0",
		Method:   "0",
		Value:    "0",
		DataType: ""}
	for _, elm := range data {
		if len(elm) != 0 {
			propval := strings.Split(elm, ":")
			switch propval[0] {
			case "class":
				event.Class = propval[1]
				break
			case "protocol":
				event.Protocol = propval[1]
				break
			case "model":
				event.Model = propval[1]
				break
			case "code":
				event.Code = propval[1]
				break
			case "house":
				event.House = propval[1]
				break
			case "unit":
				event.Unit = propval[1]
				break
			case "group":
				event.Group = propval[1]
				break
			case "method":
				event.Method = propval[1]
				break
			case "id":
				event.Id = propval[1]
				break
			case "temp":
				event.Temp = propval[1]
				break
			case "humidity":
				event.Humidity = propval[1]
				break
			}
		} else {
			var topicTemplate string
			var payloadTemplate string
			if event.Class == "command" {
				topicTemplate = viper.GetString("Mqtt.Events.PublishTopic")
				payloadTemplate = viper.GetString("Mqtt.Events.PublishPayload")

				turnOn := viper.GetString("Tellstick.MapTurnOnTo")
				turnOff := viper.GetString("Tellstick.MapTurnOffTo")

				if len(turnOn) > 0 && event.Method == "turnon" {
					event.Method = turnOn
				}

				if len(turnOff) > 0 && event.Method == "turnoff" {
					event.Method = turnOff
				}
			} else {
				topicTemplate = viper.GetString("Mqtt.Sensors.PublishTopic")
				payloadTemplate = viper.GetString("Mqtt.Sensors.PublishPayload")
				event.Value = event.Temp
				event.DataType = "temp"
			}

			var topicString string
			var payloadString string

			topicString = parseTemplate(topicTemplate, event)
			payloadString = parseTemplate(payloadTemplate, event)

			var token MQTT.Token

			log.Printf("Publish to '%s' with '%s'\n", topicString, payloadString)
			token = mqttClient.Publish(topicString, 0, false, payloadString)
			token.Wait()

			// Send a duplicate event for humidity
			if viper.GetBool("Tellstick.SplitTemperatureAndHumidity") && event.Class == "sensor" {
				event.DataType = "humidity"
				event.Value = event.Humidity
				topicString = parseTemplate(topicTemplate, event)
				payloadString = parseTemplate(payloadTemplate, event)

				log.Printf("Publish to '%s' with '%s'\n", topicString, payloadString)
				token = mqttClient.Publish(topicString, 0, false, payloadString)
				token.Wait()
			}
		}
	}
}

func handleTelldusDeviceIDEvent(event TellstickMQTTBrokerEvent) {
	var result int

	if viper.GetBool("Tellstick.ReverseMappingOnIncoming") {
		turnOn := viper.GetString("Tellstick.MapTurnOnTo")
		turnOff := viper.GetString("Tellstick.MapTurnOffTo")

		if len(turnOn) > 0 && event.Method == turnOn {
			event.Method = "turnon"
		}

		if len(turnOff) > 0 && event.Method == turnOff {
			event.Method = "turnoff"
		}
	}

	switch event.Method {
	case tellduscore.TellstickTurnoffString:
		if telldusSocket != nil {
			result = tellduscore.GetIntFromResult(sendTelldusDeviceEvent(tellduscore.GetTellstickMessage("tdTurnOff", event.DeviceID)))
		}
		log.Printf("Tellstick turn off: %s (%d)\n", tellduscore.GetResultMessage(result), result)
		break
	case tellduscore.TellstickTurnonString:
		if telldusSocket != nil {
			result = tellduscore.GetIntFromResult(sendTelldusDeviceEvent(tellduscore.GetTellstickMessage("tdTurnOn", event.DeviceID)))
		}
		log.Printf("Tellstick turn on: %s (%d)\n", tellduscore.GetResultMessage(result), result)
		break
	case tellduscore.TellstickLearnString:
		if telldusSocket != nil {
			result = tellduscore.GetIntFromResult(sendTelldusDeviceEvent(tellduscore.GetTellstickMessage("tdLearn", event.DeviceID)))
		}
		log.Printf("Tellstick learn: %s (%d)\n", tellduscore.GetResultMessage(result), result)
		break
	case tellduscore.TellstickDimString:
		if telldusSocket != nil {
			result = tellduscore.GetIntFromResult(sendTelldusDeviceEvent(tellduscore.GetTellstickMessageLevel("tdDim", event.DeviceID, event.Level)))
		}
		log.Printf("Tellstick dim: %s (%d)\n", tellduscore.GetResultMessage(result), result)
		break
	default:
		log.Printf("Unknown tellstick method: %s\n", event.Method)
	}
}

func onMessageReceived(client MQTT.Client, message MQTT.Message) {
	//log.Printf("Received message on topic: %s\nMessage: %s\n", message.Topic(), message.Payload())

	var event TellstickMQTTBrokerEvent
	err := json.Unmarshal(message.Payload(), &event)
	if err != nil {
		fmt.Println("error:", err)
	}
	log.Printf("Transmit event requested: %+v\n", event)

	if event.Protocol == "telldusdevice" {
		handleTelldusDeviceIDEvent(event)
		return
	}

	/*
		if event.Protocol != "archtech" {
			log.Printf("Unsupported protocol: %s\n", event.Protocol)
			return
		}

		method := 0
		switch event.Method {
		case tellduscore.TellstickTurnoffString:
			method = tellduscore.TellstickTurnoff
			break
		case tellduscore.TellstickTurnonString:
			method = tellduscore.TellstickTurnon
			break
		case tellduscore.TellstickDimString:
			method = tellduscore.TellstickDim
			break
		}

		rawCommand := tellduscore.GetRawCommand(event.House, event.Unit, method, event.Level)
		log.Printf("Translated to: %02X\n", rawCommand)
		tellResult := C.tdSendRawCommand(C.CString(rawCommand), 0)

		if tellResult != 0 { // !TELLSTICK_SUCCESS
			resultType := tellduscore.GetResultMessage(int(tellResult))

			log.Printf("Error transmitting command: (%d) %s", tellResult, resultType)
		} else {
			log.Println("Tellstick reports success.")
		}
	*/
}

func onTellstickDeviceMessageReceived(client MQTT.Client, message MQTT.Message) {
	topic := message.Topic()
	topicElements := strings.Split(topic, "/")
	deviceIDString := topicElements[len(topicElements)-1]
	deviceID, err := strconv.Atoi(deviceIDString)

	if err != nil {
		log.Fatalf("Error parsing device id '%s': %v\n", deviceIDString, err)
		return
	}

	event := TellstickMQTTBrokerEvent{
		DeviceID: deviceID,
		Method:   string(message.Payload()),
		Protocol: "telldusdevice"}

	if event.Method == tellduscore.TellstickDimString {
		dimLevelString := string(message.Payload())
		dimLevel, err := strconv.Atoi(dimLevelString)
		if err != nil {
			log.Fatalf("Error parsing dim level '%s': %v\n", dimLevelString, err)
			return
		}
		event.Level = dimLevel
	}

	handleTelldusDeviceIDEvent(event)
}

func parseTemplate(templateString string, event *TelldusEvent) string {
	tmpl, err := template.New("template").Parse(templateString)
	if err != nil {
		log.Panicf("Error parsing template: %v", err)
	}

	var tmplBuffer bytes.Buffer
	tmplWriter := io.Writer(&tmplBuffer)
	err = tmpl.Execute(tmplWriter, event)

	if err != nil {
		log.Panicf("Error executing template: %v", err)
	}

	return tmplBuffer.String()
}

func createTLSConfig(cafile string) *tls.Config {
	certpool := x509.NewCertPool()
	ca, err := ioutil.ReadFile(cafile)
	if err != nil {
		log.Panicf("Unable to read CA certificate: %v", err)
	}
	certpool.AppendCertsFromPEM(ca)

	tlsConf := &tls.Config{
		RootCAs: certpool,
		ClientAuth: tls.NoClientCert,
		ClientCAs: nil,
		InsecureSkipVerify: false,
		MinVersion: tls.VersionTLS12,
		// Certificates: []tls.Certificate{clientCert},
	}

	return tlsConf
}

func setupMqtt() {
	opts := MQTT.NewClientOptions()

	opts.AddBroker(viper.GetString("Mqtt.Broker"))
	opts.SetClientID(viper.GetString("Mqtt.ClientId"))
	opts.SetUsername(viper.GetString("Mqtt.Username"))
	opts.SetPassword(viper.GetString("Mqtt.Password"))

	if(viper.IsSet("Mqtt.CACert")) {
		cafile := viper.GetString("Mqtt.CACert")
		log.Printf("TLS CACert %v", cafile)
		tlsConfig := createTLSConfig(cafile)
		opts.SetTLSConfig(tlsConfig)
	}

	topic := viper.GetString("Mqtt.Events.SubscribeTopic")
	deviceTopic := viper.GetString("Mqtt.Events.SubscribeDeviceEvents")
	qos := 0

	opts.OnConnect = func(c MQTT.Client) {
		log.Printf("Subscribing to: %s\n", topic)
		if token := c.Subscribe(topic, byte(qos), onMessageReceived); token.Wait() && token.Error() != nil {
			log.Panicf("Unable to subscribe to topic: %v", token.Error())
		}

		log.Printf("Subscribing to: %s\n", deviceTopic)
		if token := c.Subscribe(deviceTopic, byte(qos), onTellstickDeviceMessageReceived); token.Wait() && token.Error() != nil {
			log.Panicf("Unable to subscribe to topic: %v", token.Error())
		}
	}

	mqttClient = MQTT.NewClient(opts)
	if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
		log.Panicf("Unable to connect to MQTT: %v", token.Error())
	}

}

func setupConfiguration() {
	viper.SetConfigName("telldusmq")
	viper.AddConfigPath("/etc/telldusmq/")
	viper.AddConfigPath("$HOME/.telldusmq/")
	viper.AddConfigPath("./")

	configError := viper.ReadInConfig()
	if configError != nil {
		log.Panicf("Error reading configuration: %v\n", configError)
	}

	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		log.Println("Reloading configuration", e.Name)
		// TODO: Reconnect mqtt broker if connection params changes?
	})
}

func main() {
	log.Println("Started Message Queue for Telldus Core")
	setupConfiguration()
	setupMqtt()
	go setupTelldus()

	for {
		time.Sleep(30 * time.Second)
	}
}
