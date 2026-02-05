package evo.developers.ru.client.bot;

import com.fasterxml.jackson.databind.ObjectMapper;
import evo.developers.ru.client.crypto.FastClient;
import org.telegram.telegrambots.bots.TelegramLongPollingBot;
import org.telegram.telegrambots.meta.TelegramBotsApi;
import org.telegram.telegrambots.meta.api.methods.send.SendMessage;
import org.telegram.telegrambots.meta.api.objects.Update;
import org.telegram.telegrambots.meta.api.objects.replykeyboard.ReplyKeyboardMarkup;
import org.telegram.telegrambots.meta.api.objects.replykeyboard.buttons.KeyboardButton;
import org.telegram.telegrambots.meta.api.objects.replykeyboard.buttons.KeyboardRow;
import org.telegram.telegrambots.updatesreceivers.DefaultBotSession;

import java.io.File;
import java.io.IOException;
import java.net.HttpURLConnection;
import java.net.URISyntaxException;
import java.net.URL;
import java.util.*;

public class FastClientBot extends TelegramLongPollingBot {

    private final FastClient client = new FastClient();
    private Set<Long> admins = new HashSet<>();
    private final ObjectMapper mapper = new ObjectMapper();

    private File configFile = new File("bot_config.json");
    private Config config = new Config();

    private final Map<Long, PendingAction> pendingAction = new HashMap<>();
    private MessageManager messageManager;

    public static void main(String[] args) throws Exception {
        TelegramBotsApi botsApi = new TelegramBotsApi(DefaultBotSession.class);
        botsApi.registerBot(new FastClientBot());
        System.out.println("Bot started!");
    }

    public FastClientBot() throws URISyntaxException {
        configFile = new File("./bot_config.json");

        loadConfig();
        admins = new HashSet<>(config.admins);
        
        messageManager = new MessageManager(this, config.chatId);
        
        client.setAccessToken(config.accessToken);
        client.setMinAmount(config.minAmount);
        client.setMaxAmount(config.maxAmount);
        client.setUsingParalles(config.usingParallelRequests);

        client.setListener(message -> messageManager.sendPlainMessage(message));

        if (!config.accessToken.isEmpty() && checkToken(config.accessToken)) {
            System.out.println("✅ Токен валиден");
        } else if (!config.accessToken.isEmpty()) {
            System.out.println("❌ Токен недействителен");
        } else {
            System.out.println("⚠️ Токен не установлен");
        }
    }


    @Override
    public String getBotToken() {
        return config.botToken;
    }

    @Override
    public String getBotUsername() {
        return config.botName;
    }

    @Override
    public void onUpdateReceived(Update update) {
        if (!update.hasMessage() || !update.getMessage().hasText()) return;

        long chatId = update.getMessage().getChatId();
        long userId = update.getMessage().getFrom().getId();
        String text = update.getMessage().getText().trim();

        if (!admins.contains(userId)) {
            send(chatId, "⛔️ Доступ запрещён!");
            return;
        }

        if (pendingAction.containsKey(userId)) {
            handlePendingAction(chatId, userId, text);
            return;
        }

        switch (text) {
            case "/start":
            case "⬅️ Главное меню":
                send(chatId, "Привет! Выберите действие:", getMainKeyboard());
                break;
            case "🚀 Запустить клиент":
                client.start();
                send(chatId, "🚀 Клиент запущен!", getMainKeyboard());
                break;
            case "🛑 Остановить клиент":
                client.stop();
                send(chatId, "🛑 Клиент остановлен!", getMainKeyboard());
                break;
            case "ℹ️ Статус":
                String statusMsg = "📊 Статус системы:\n\n";
                statusMsg += "🤖 FastClient:\n";
                statusMsg += "ℹ️ Работает: " + client.isRunning() + "\n";
                statusMsg += "🔑 Токен валиден: " + checkToken(config.accessToken) + "\n";
                statusMsg += "📉 Мин. сумма: " + config.minAmount + "\n";
                statusMsg += "📈 Макс. сумма: " + config.maxAmount + "\n";
                statusMsg += "💬 Chat ID: " + config.chatId + "\n";
                statusMsg += "🔀 Параллельность запросов(приводит к блокировке аккаунта, 5 одновременных запросов, асинхронных! Но повышает шанс забрать заказ!): " + (config.usingParallelRequests ? "🔀 Включено" : "❌ Отключено") + "\n";

                sendPlainText(chatId, statusMsg, getMainKeyboard());
                break;
            case "⚙️ Настройки":
                send(chatId, "Выберите параметр для изменения:", getSettingsKeyboard());
                break;
            case "🔑 Установить токен":
                pendingAction.put(userId, new PendingAction(ActionType.SET_TOKEN));
                send(chatId, "Введите новый токен:");
                break;
            case "📉 Изменить минимальную сумму":
                pendingAction.put(userId, new PendingAction(ActionType.SET_MIN));
                send(chatId, "Введите минимальную сумму:");
                break;
            case "📈 Изменить максимальную сумму":
                pendingAction.put(userId, new PendingAction(ActionType.SET_MAX));
                send(chatId, "Введите максимальную сумму:");
                break;
            case "💬 Изменить Chat ID":
                pendingAction.put(userId, new PendingAction(ActionType.SET_CHAT));
                send(chatId, "Введите новый Chat ID:");
                break;
            case "🔀 Включить параллельные запросы":
                config.usingParallelRequests = true;
                client.setUsingParalles(true);
                saveConfig();
                send(chatId, "✅Паралельные потоки включены!", getMainKeyboard());
                break;
            case "🚫|| Отключить параллельные запросы":
                config.usingParallelRequests = false;
                client.setUsingParalles(false);
                saveConfig();
                send(chatId, "⚠️ Паралельные потоки выключены!", getMainKeyboard());
                break;
            default:
                send(chatId, "❓ Неизвестная команда. Используйте кнопки снизу.", getMainKeyboard());
        }
    }

    private void handlePendingAction(long chatId, long userId, String text) {
        PendingAction action = pendingAction.get(userId);
        try {
            switch (action.type) {
                case SET_TOKEN:
                    if (checkToken(text)) {
                        config.accessToken = text;
                        client.setAccessToken(text);
                        saveConfig();
                        send(chatId, "✅ Токен установлен и валиден!", getSettingsKeyboard());
                    } else {
                        send(chatId, "❌ Токен недействителен! Попробуйте ещё раз:", null);
                        return;
                    }
                    break;
                case SET_MIN:
                    double min = Double.parseDouble(text);
                    config.minAmount = min;
                    client.setMinAmount(min);
                    saveConfig();
                    send(chatId, "✅ Минимальная сумма обновлена!", getSettingsKeyboard());
                    break;
                case SET_MAX:
                    double max = Double.parseDouble(text);
                    config.maxAmount = max;
                    client.setMaxAmount(max);
                    saveConfig();
                    send(chatId, "✅ Максимальная сумма обновлена!", getSettingsKeyboard());
                    break;
                case SET_CHAT:
                    long newChatId = Long.parseLong(text);
                    config.chatId = newChatId;

                    messageManager = new MessageManager(this, newChatId);
                    saveConfig();
                    send(chatId, "✅ Chat ID обновлён!", getSettingsKeyboard());
                    break;
            }
        } catch (Exception e) {
            send(chatId, "❌ Некорректное значение! Попробуйте снова:", null);
            return;
        } finally {
            pendingAction.remove(userId);
        }
    }
    


    private ReplyKeyboardMarkup getMainKeyboard() {
        ReplyKeyboardMarkup keyboard = new ReplyKeyboardMarkup();
        keyboard.setResizeKeyboard(true);
        keyboard.setOneTimeKeyboard(false);
        List<KeyboardRow> rows = new ArrayList<>();

        KeyboardRow row1 = new KeyboardRow();
        row1.add(new KeyboardButton("🚀 Запустить клиент"));
        row1.add(new KeyboardButton("🛑 Остановить клиент"));
        rows.add(row1);

        KeyboardRow row2 = new KeyboardRow();
        row2.add(new KeyboardButton("ℹ️ Статус"));
        row2.add(new KeyboardButton("⚙️ Настройки"));
        rows.add(row2);

        keyboard.setKeyboard(rows);
        return keyboard;
    }

    private ReplyKeyboardMarkup getSettingsKeyboard() {
        ReplyKeyboardMarkup keyboard = new ReplyKeyboardMarkup();
        keyboard.setResizeKeyboard(true);
        keyboard.setOneTimeKeyboard(false);
        List<KeyboardRow> rows = new ArrayList<>();

        KeyboardRow row1 = new KeyboardRow();
        row1.add(new KeyboardButton("🔑 Установить токен"));
        rows.add(row1);

        KeyboardRow row2 = new KeyboardRow();
        row2.add(new KeyboardButton("📉 Изменить минимальную сумму"));
        row2.add(new KeyboardButton("📈 Изменить максимальную сумму"));
        rows.add(row2);

        KeyboardRow row3 = new KeyboardRow();
        row3.add(new KeyboardButton("💬 Изменить Chat ID"));
        rows.add(row3);


        KeyboardRow row4 = new KeyboardRow();
        String toggleText = config.usingParallelRequests ? "🚫|| Отключить параллельные запросы" : "🔀 Включить параллельные запросы";
        row4.add(new KeyboardButton(toggleText));
        rows.add(row4);

        KeyboardRow row5 = new KeyboardRow();
        row5.add(new KeyboardButton("⬅️ Главное меню"));
        rows.add(row5);

        keyboard.setKeyboard(rows);
        return keyboard;
    }

    private void send(long chatId, String text, ReplyKeyboardMarkup keyboard) {
        try {
            SendMessage sm = new SendMessage(String.valueOf(chatId), text);
            sm.setParseMode("Markdown");
            if (keyboard != null) sm.setReplyMarkup(keyboard);
            execute(sm);
        } catch (Exception e) {
            e.printStackTrace();
        }
    }

    private void send(long chatId, String text) {
        send(chatId, text, null);
    }
    
    private void sendPlainText(long chatId, String text, ReplyKeyboardMarkup keyboard) {
        try {
            SendMessage sm = new SendMessage(String.valueOf(chatId), text);
            if (keyboard != null) sm.setReplyMarkup(keyboard);
            execute(sm);
        } catch (Exception e) {
            e.printStackTrace();
        }
    }

    private void saveConfig() {
        try {
            mapper.writeValue(configFile, config);
        } catch (IOException e) {
            e.printStackTrace();
        }
    }

    private void loadConfig() {
        try {
            if (configFile.exists()) {
                config = mapper.readValue(configFile, Config.class);
            }
        } catch (IOException e) {
            e.printStackTrace();
        }
    }

    private boolean checkToken(String token) {
        if (token == null || token.isEmpty()) return false;
        try {
            URL url = new URL("https://app.cr.bot/internal/v1/p2c/accounts");
            HttpURLConnection con = (HttpURLConnection) url.openConnection();
            con.setRequestMethod("GET");
            con.setRequestProperty("Cookie", "access_token=" + token);
            con.setRequestProperty("Accept", "application/json");
            int status = con.getResponseCode();
            con.disconnect();
            return status == 200;
        } catch (Exception e) {
            e.printStackTrace();
            return false;
        }
    }

    private static class Config {
        public String botToken = "";
        public String botName = "crypto_call5_bot";
        public long chatId = -1002966994571L;
        public String accessToken = "";
        public double minAmount = 0;
        public double maxAmount = 0;
        public List<Long> admins = new ArrayList<>();


        public boolean usingParallelRequests = false;
    }

    private enum ActionType {
        SET_TOKEN, SET_MIN, SET_MAX, SET_CHAT
    }

    private static class PendingAction {
        public ActionType type;
        public PendingAction(ActionType type) {
            this.type = type;
        }
    }
}
