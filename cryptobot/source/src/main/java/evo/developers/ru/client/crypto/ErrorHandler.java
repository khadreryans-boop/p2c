package evo.developers.ru.client.crypto;


import java.io.IOException;
import java.net.ConnectException;
import java.net.SocketTimeoutException;


public class ErrorHandler {
    
    public interface ErrorListener {
        void onError(String userMessage, String technicalDetails, ErrorSeverity severity);
    }

    public enum ErrorSeverity {
        INFO("ℹ️"),
        WARNING("⚠️"),
        ERROR("❌"),
        CRITICAL("🔴");
        
        private final String emoji;
        
        ErrorSeverity(String emoji) {
            this.emoji = emoji;
        }
        
        public String getEmoji() {
            return emoji;
        }
    }
    
    private final ErrorListener listener;
    
    public ErrorHandler(ErrorListener listener) {
        this.listener = listener;
    }
    

    public void handle(Exception e) {
        handle(e, null);
    }

    public void handle(Exception e, String context) {
        ErrorInfo errorInfo = analyzeError(e);
        
        String userMessage = errorInfo.userMessage;
        if (context != null && !context.isEmpty()) {
            userMessage = context + ": " + userMessage;
        }
        
        listener.onError(
            errorInfo.severity.getEmoji() + " " + userMessage,
            e.getClass().getSimpleName() + ": " + e.getMessage(),
            errorInfo.severity
        );


        e.printStackTrace();
    }
    

    private ErrorInfo analyzeError(Exception e) {

        if (e instanceof SocketTimeoutException) {
            return new ErrorInfo(
                "Превышено время ожидания ответа",
                ErrorSeverity.WARNING
            );
        }
        
        if (e instanceof ConnectException) {
            return new ErrorInfo(
                "Ошибка подключения к серверу",
                ErrorSeverity.ERROR
            );
        }
        
        if (e instanceof IOException) {
            return new ErrorInfo(
                "Ошибка ввода-вывода: " + e.getMessage(),
                ErrorSeverity.ERROR
            );
        }
        
        if (e.getClass().getSimpleName().contains("JSON")) {
            return new ErrorInfo(
                "Ошибка обработки данных",
                ErrorSeverity.WARNING
            );
        }
        
        String message = e.getMessage();
        if (message != null) {
            if (message.contains("401") || message.contains("Unauthorized")) {
                return new ErrorInfo(
                    "Ошибка авторизации. Проверьте токены",
                    ErrorSeverity.CRITICAL
                );
            }
            
            if (message.contains("403") || message.contains("Forbidden")) {
                return new ErrorInfo(
                    "Доступ запрещен",
                    ErrorSeverity.ERROR
                );
            }
            
            if (message.contains("429") || message.contains("Too Many Requests")) {
                return new ErrorInfo(
                    "Слишком много запросов. Подождите немного",
                    ErrorSeverity.WARNING
                );
            }
            
            if (message.contains("500") || message.contains("Internal Server Error")) {
                return new ErrorInfo(
                    "Ошибка сервера. Попробуйте позже",
                    ErrorSeverity.ERROR
                );
            }
        }
        

        return new ErrorInfo(
            "Неизвестная ошибка: " + (message != null ? message : e.getClass().getSimpleName()),
            ErrorSeverity.ERROR
        );
    }
    

    public static String formatError(String message, ErrorSeverity severity) {
        return severity.getEmoji() + " " + message;
    }
    

    private static class ErrorInfo {
        final String userMessage;
        final ErrorSeverity severity;
        
        ErrorInfo(String userMessage, ErrorSeverity severity) {
            this.userMessage = userMessage;
            this.severity = severity;
        }
    }
}



