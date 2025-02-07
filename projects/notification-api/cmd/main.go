package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/websocket"
	"github.com/segmentio/kafka-go"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	clients   = make(map[*websocket.Conn]bool)
	broadcast = make(chan string)
	mutex     sync.Mutex
	upgrader  = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	mongoClient *mongo.Client
)

func main() {
	ctx := context.Background()
	var err error

	mongoURI := getEnv("MONGO_URI", "mongodb://mongo:27017")
	kafkaBrokers := getEnv("KAFKA_BROKERS", "kafka:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "notifications")
	serverPort := getEnv("SERVER_PORT", "8080")

	log.Println("Connecting to MongoDB at", mongoURI)
	mongoClient, err = mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatal("Erro ao conectar ao MongoDB:", err)
	}
	defer mongoClient.Disconnect(ctx)

	log.Println("Iniciando consumidor Kafka...")
	go kafkaConsumer(kafkaBrokers, kafkaTopic)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Get("/ws", handleConnections)
	r.Post("/notify", func(w http.ResponseWriter, r *http.Request) {
		log.Println("Recebendo nova notificação via HTTP...")
		handleNotification(w, r, kafkaBrokers, kafkaTopic)
	})

	log.Println("Servidor iniciado na porta", serverPort)
	http.ListenAndServe(":"+serverPort, r)
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	log.Println("Nova conexão WebSocket recebida...")
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Erro ao abrir conexão WebSocket:", err)
		return
	}
	defer conn.Close()

	mutex.Lock()
	clients[conn] = true
	mutex.Unlock()

	log.Println("Conexão WebSocket estabelecida com sucesso")
	for msg := range broadcast {
		conn.WriteMessage(websocket.TextMessage, []byte(msg))
	}
}

func handleNotification(w http.ResponseWriter, r *http.Request, kafkaBrokers, kafkaTopic string) {
	var msg struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		log.Println("Erro ao decodificar JSON da notificação:", err)
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	log.Println("Enviando mensagem para Kafka...")
	producer := kafka.NewWriter(kafka.WriterConfig{
		Brokers:  []string{kafkaBrokers},
		Topic:    kafkaTopic,
		Balancer: &kafka.LeastBytes{},
	})
	defer producer.Close()

	err := producer.WriteMessages(context.Background(), kafka.Message{Value: []byte(msg.Message)})
	if err != nil {
		log.Println("Erro ao enviar mensagem para Kafka:", err)
		http.Error(w, "Kafka producer error", http.StatusInternalServerError)
		return
	}

	log.Println("Mensagem enviada com sucesso para Kafka")
	w.WriteHeader(http.StatusNoContent)
}

func kafkaConsumer(kafkaBrokers, kafkaTopic string) {
	log.Println("Iniciando leitor Kafka para o tópico", kafkaTopic)
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers: []string{kafkaBrokers},
		Topic:   kafkaTopic,
		GroupID: "notification-service",
	})
	defer r.Close()

	for {
		msg, err := r.ReadMessage(context.Background())
		if err != nil {
			log.Println("Erro ao ler mensagem Kafka:", err)
			continue
		}
		log.Printf("Nova notificação recebida do Kafka: %s", string(msg.Value))

		db := mongoClient.Database("notifications").Collection("messages")
		_, err = db.InsertOne(context.Background(), bson.M{"message": string(msg.Value)})
		if err != nil {
			log.Println("Erro ao armazenar notificação no MongoDB:", err)
		} else {
			log.Println("Notificação armazenada no MongoDB com sucesso")
		}

		log.Println("Enviando notificação para WebSocket...")
		broadcast <- string(msg.Value)
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
