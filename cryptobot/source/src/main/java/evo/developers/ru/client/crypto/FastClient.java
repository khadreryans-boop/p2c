package evo.developers.ru.client.crypto;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import io.socket.client.IO;
import io.socket.client.Socket;
import lombok.Data;
import lombok.Getter;
import lombok.Setter;
import okhttp3.*;
import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.concurrent.Executor;
import java.util.concurrent.Executors;

@Data
public class FastClient {

    private final String baseUrl = "https://app.cr.bot";
    private String accessToken = "";
    private double minAmount = 1;
    private double maxAmount = 10000;
    private boolean autoCloseOrderInSystem = false;
    private Socket socket;
    private final Executor executor = Executors.newVirtualThreadPerTaskExecutor();
    private final HttpClient httpClient = HttpClient.newBuilder().executor(executor).build();
    private boolean isSocketRunning = false;
    private ClientListener listener;
    private ErrorHandler errorHandler;

    @Setter
    private String pinCode;
    @Setter
    private String userAgent;

    @Setter
    @Getter
    private boolean usingParalles;

    public interface ClientListener {
        void onSystemLog(String message);
    }

    public FastClient() {
        this.errorHandler = new ErrorHandler((userMessage, technicalDetails, severity) -> {
            if (listener != null) {
                listener.onSystemLog(userMessage);
            }
            System.err.println("[" + severity + "] " + userMessage);
            System.err.println("    Technical: " + technicalDetails);
        });
    }

    public boolean isRunning() {
        return isSocketRunning;
    }

    private int maxAppendRequest;

    public void start() {
        if (isSocketRunning) return;
        try {
            IO.Options options = new IO.Options();
            options.path = "/internal/v1/p2c-socket";
            options.transports = new String[]{"websocket"};
            options.extraHeaders = java.util.Map.of(
                    "Cookie", java.util.List.of("access_token=" + accessToken),
                    "User-Agent", java.util.List.of("Java-HttpClient")
            );

            socket = IO.socket(baseUrl, options);

            socket.on(Socket.EVENT_CONNECT, args -> {

                maxAppendRequest = usingParalles ? 5 : 1;
                logSystem("🔌 WebSocket подключен: %s %s".
                        formatted(
                                socket.id(),
                                usingParalles ? "\n⚡ Параллельность включена" : ""
                        ));
                socket.emit("list:initialize");
            });
            
            socket.on(Socket.EVENT_DISCONNECT, args -> 
                logSystem("🔌 WebSocket отключен"));
            
            socket.on(Socket.EVENT_CONNECT_ERROR, args -> 
                logSystem("❌ Ошибка подключения WebSocket: " + args[0]));

            socket.on("list:update", args -> {
                JSONArray payload = (JSONArray) args[0];
                for (int i = 0; i < payload.length(); i++) {
                    JSONObject data = payload.optJSONObject(i)
                            .optJSONObject("data");
                    if (data == null) continue;

                    double inAmount = data.optDouble("in_amount", 0);
                    if (inAmount >= minAmount && inAmount <= maxAmount) {
                        String paymentId = data.optString("id");
                        try {

                            for (int x = 0; x < maxAppendRequest; x++) {
                                Thread.startVirtualThread(() -> {
                                    try {
                                        takePayment(paymentId);
                                    } catch (Exception e) {
                                        errorHandler.handle(e, "Ошибка захвата заказа");
                                    }
                                });
                            }
                        } catch (Exception e) {
                            errorHandler.handle(e, "Ошибка запуска обработки заказа");
                        }
                        //break;
                    }
                }
            });
            
            socket.connect();
            isSocketRunning = true;
            
        } catch (Exception e) {
            errorHandler.handle(e, "Ошибка запуска клиента");
        }
    }

    public void stop() {
        if (socket != null) {
            socket.disconnect();
            socket.close();
            socket = null;
        }
        isSocketRunning = false;
    }

    private void logSystem(String msg) {
        if (listener != null) {
            listener.onSystemLog(msg);
        }
        System.out.println(msg);
    }

    private void logDebug(String msg) {
        System.out.println("[DEBUG] " + msg);
    }

    private void takePayment(String paymentHash) {
        try {
            HttpRequest request = HttpRequest.newBuilder()
                    .uri(URI.create(baseUrl + "/internal/v1/p2c/payments/take/" + paymentHash))
                    .header("accept", "application/json")
                    .header("cookie", "access_token=" + accessToken)
                    .header("user-agent", "Java-HttpClient")
                    .POST(HttpRequest.BodyPublishers.noBody())
                    .build();

            httpClient.sendAsync(request, HttpResponse.BodyHandlers.ofString())
                    .thenApply(HttpResponse::body)
                    .thenApply(this::safeJson)
                    .thenAccept(res -> {
                        if (!res.has("error")) {
                            try {
                                JSONObject data = res.getJSONObject("data");
                                long orderId = data.getLong("id");

                                logSystem("✅ Заказ #" + orderId + " успешно захвачен");

                            } catch (JSONException e) {
                                errorHandler.handle(e, "Ошибка парсинга данных заказа");
                            }
                        } else {
                            logDebug("Failed to capture order: " + paymentHash);
                        }
                    })
                    .exceptionally(ex -> {
                        errorHandler.handle((Exception) ex, "HTTP error while capturing order");
                        return null;
                    });
                    
        } catch (Exception e) {
            errorHandler.handle(e, "Исключение при захвате заказа");
        }
    }

    private JSONObject safeJson(String body) {
        try {
            return new JSONObject(body);
        } catch (Exception e) {
            JSONObject err = new JSONObject();
            try {
                err.put("error", "Invalid JSON");
            } catch (Exception ignored) {
            }
            return err;
        }
    }
}
