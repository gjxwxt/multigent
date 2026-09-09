package com.example.app.model;

public record HealthResponse(
        String status,
        String timestamp,
        String service
) {}
