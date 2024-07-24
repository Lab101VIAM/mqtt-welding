package mqttclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Init called upon import, registers this component with the module
func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{Constructor: newSensor})
}

// To be used for functions which are not meant to be implemented in your component
var errUnimplemented = errors.New("unimplemented")

// Your model's colon-delimited-triplet (acme:demo:mybase). acme = namespace, demo = repo-name, mybase = model name
// If you plan to upload this module to the Viam registry, "acme" must match your Viam registry namespace.
var Model = resource.NewModel("viam-soleng", "mqtt", "client")

// Maps JSON component configuration attributes.
type Config struct {
	Topic       string `json:"topic"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	QoS         int    `json:"qos"`
	QueueLength int    `json:"q_length"`
	ClientID    string `json:"clientid"`
	MessageType string `json:"msg_type"` // Supported json, string, raw (default)
}

// Implement component configuration validation and and return implicit dependencies.
func (cfg *Config) Validate(path string) ([]string, error) {
	// Check if the topic is set
	if cfg.Topic == "" {
		return nil, fmt.Errorf("topic is required %q", path)
	}

	// Check if the host is set
	if cfg.Host == "" {
		return nil, fmt.Errorf("host is required %q", path)
	}

	// Check if the port is valid
	if cfg.Port <= 0 {
		return nil, fmt.Errorf("invalid port (should be > 0) %q", path)
	}

	// Check if qos is within a valid range (usually 0 to 2 for MQTT)
	if cfg.QoS < 0 || cfg.QoS > 2 {
		return nil, fmt.Errorf("qos must be between 0 and 2 %q", path)
	}

	switch cfg.MessageType {
	case "", "json", "string", "raw":
	default:
		return nil, fmt.Errorf(`message type must be either "", "json", "string", or "raw"`)
	}

	return []string{}, nil
}

type mqttClient struct {
	resource.Named
	logger        logging.Logger
	client        mqtt.Client
	Topic         string
	Host          string
	Port          int
	QoS           byte
	ClientID      string
	messageType   string
	messageQueue  []mqtt.Message
	queueLength   int
	latestMessage mqtt.Message
	mutex         sync.Mutex
}

// Sensor type constructor.
// Called upon sensor instantiation when a sensor model is added to the machine configuration
func newSensor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	s := &mqttClient{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}
	if err := s.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return s, nil
}

// Reconfigure reconfigures with new settings.
func (s *mqttClient) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	// Convert the generic resource.Config to the MQTT_Client-specific Config structure
	clientConfig, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}

	// Stop existing MQTT client if connected
	if s.client != nil && s.client.IsConnected() {
		s.client.Disconnect(250) // Timeout in milliseconds
	}

	// Reconfigure the MQTT_Client instance with new settings from clientConfig
	s.Topic = clientConfig.Topic
	s.Host = clientConfig.Host
	s.Port = clientConfig.Port
	s.QoS = byte(clientConfig.QoS) // Assuming qos in Config is an int and needs conversion to byte
	s.queueLength = clientConfig.QueueLength
	s.ClientID = clientConfig.ClientID
	s.messageType = clientConfig.MessageType
	// Log the new configuration (optional, adjust logging as needed)
	s.logger.Infof("Reconfigured mqtt client with topic: %s, host: %s, port: %d, qos: %d, clientID: %s, msgtype: %s, q_length: %v", s.Topic, s.Host, s.Port, s.QoS, s.ClientID, s.messageType, s.queueLength)

	// Error handling channel
	errChan := make(chan error, 1)

	// Start InitMQTTClient in a goroutine
	go func() {
		errChan <- s.InitMQTTClient(ctx)
		close(errChan)
	}()

	// Handle errors from the goroutine
	for err := range errChan {
		if err != nil {
			// Handle error, e.g., log it or restart the initialization process
			s.logger.Errorf("Error initializing mqtt client: %v", err)
			// Take appropriate action based on the error
		}
	}

	return err
}

// Get sensor reading
func (s *mqttClient) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	// If Viam data manager return the latest message if the message queue is not empty and remove it from the queue
	if extra[data.FromDMString] == true {
		if len(s.messageQueue) != 0 {
			oldestMessage := s.messageQueue[0]
			s.messageQueue = s.messageQueue[1:]
			parsedPayload, err := parsePayload(s.messageType, oldestMessage)
			if err != nil {
				s.logger.Error(err)
				return nil, data.ErrNoCaptureToStore
			}
			return map[string]interface{}{
				"payload": parsedPayload,
				"qos":     int32(s.QoS),
				"topic":   s.Topic,
			}, nil
		} else {
			return nil, data.ErrNoCaptureToStore
		}
	}
	// If not data manager return the latest message
	// Check if there have been any messages received
	if s.latestMessage != nil {
		parsedPayload, err := parsePayload(s.messageType, s.latestMessage)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"payload": parsedPayload,
			"qos":     int32(s.QoS),
			"topic":   s.Topic,
		}, nil

	} else {
		return nil, nil
	}

}

// Parse mqtt message
func parsePayload(mtype string, msg mqtt.Message) (interface{}, error) {
	var payload interface{}
	switch mtype {
	case "json":
		err := json.Unmarshal(msg.Payload(), &payload)
		if err != nil {
			//s.logger.Errorf("error parsing JSON message:", err)
			return nil, fmt.Errorf("error parsing JSON message: %v", err)
		}
	case "string":
		payload = string(msg.Payload())
	default:
		payload = msg.Payload()
	}
	return payload, nil
}

// DoCommand can be implemented to extend sensor functionality but returns unimplemented in this example.
func (s *mqttClient) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, errUnimplemented
}

// New function to initialize MQTT client and start the goroutine
func (s *mqttClient) InitMQTTClient(ctx context.Context) error {
	// Create a client and connect to the broker
	opts := mqtt.NewClientOptions()
	opts.AddBroker(fmt.Sprintf("tcp://%s:%d", s.Host, s.Port))
	opts.SetClientID(s.ClientID) // Set a unique client ID

	s.client = mqtt.NewClient(opts)
	if token := s.client.Connect(); token.Wait() && token.Error() != nil {
		return token.Error()
	}

	// Start the goroutine to listen to the topic
	go func() {
		if token := s.client.Subscribe(s.Topic, s.QoS, func(client mqtt.Client, msg mqtt.Message) {
			s.mutex.Lock()
			defer s.mutex.Unlock()

			// TODO: use flag instead of duplicating messages
			s.latestMessage = msg
			s.logger.Infof("message queue length: %v", len(s.messageQueue))
			if len(s.messageQueue) == s.queueLength {
				s.messageQueue = s.messageQueue[1:]
				s.messageQueue = append(s.messageQueue, msg)
			}
			s.messageQueue = append(s.messageQueue, msg)

		}); token.Wait() && token.Error() != nil {
			// Handle subscription error
			s.logger.Errorf("subscription error:", token.Error())
		}
	}()

	return nil
}

// Add a Close method to clean up the MQTT client
func (s *mqttClient) Close(ctx context.Context) error {
	if s.client != nil && s.client.IsConnected() {
		s.client.Disconnect(250) // Timeout in milliseconds
	}
	return nil
}
