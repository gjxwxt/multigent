package com.example.app.exception;

public class ResourceNotFoundException extends RuntimeException {

    public ResourceNotFoundException(String message) {
        super(message);
    }

    public ResourceNotFoundException(String resourceType, String id) {
        super(String.format("%s with id '%s' was not found", resourceType, id));
    }
}
