package com.example.app.exception;

import java.time.Instant;
import java.util.List;

public record ApiErrorResponse(
        String code,
        String message,
        List<String> details,
        Instant timestamp
) {
    public static ApiErrorResponse of(String code, String message, List<String> details) {
        return new ApiErrorResponse(code, message, details != null ? details : List.of(), Instant.now());
    }

    public static ApiErrorResponse of(String code, String message) {
        return of(code, message, List.of());
    }
}
