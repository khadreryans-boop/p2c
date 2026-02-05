package evo.developers.ru.client.bot;

import org.telegram.telegrambots.bots.TelegramLongPollingBot;
import org.telegram.telegrambots.meta.api.methods.updatingmessages.EditMessageText;
import org.telegram.telegrambots.meta.api.objects.Message;

import java.time.LocalDateTime;
import java.time.format.DateTimeFormatter;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

public class MessageManager {
    
    private final TelegramLongPollingBot bot;
    private final long chatId;
    private final Map<Long, Integer> orderMessages = new ConcurrentHashMap<>();
    private static final DateTimeFormatter TIME_FORMAT = DateTimeFormatter.ofPattern("HH:mm:ss");
    
    public MessageManager(TelegramLongPollingBot bot, long chatId) {
        this.bot = bot;
        this.chatId = chatId;
    }
    

    public void sendPlainMessage(String text) {
        try {
            org.telegram.telegrambots.meta.api.methods.send.SendMessage sendMessage = 
                new org.telegram.telegrambots.meta.api.methods.send.SendMessage();
            sendMessage.setChatId(String.valueOf(chatId));
            sendMessage.setText(text);
            bot.execute(sendMessage);
        } catch (Exception e) {
            System.err.println("❌ Ошибка отправки сообщения: " + e.getMessage());
        }
    }
}




