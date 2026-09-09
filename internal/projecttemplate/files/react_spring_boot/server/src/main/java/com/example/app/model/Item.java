package com.example.app.model;

import java.time.Instant;

public record Item(
        String id,
        String title,
        String description,
        String status,
        Instant createdAt
) {}
