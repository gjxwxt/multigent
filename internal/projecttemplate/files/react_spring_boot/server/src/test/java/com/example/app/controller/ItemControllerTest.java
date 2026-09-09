package com.example.app.controller;

import com.example.app.exception.GlobalExceptionHandler;
import com.example.app.exception.ResourceNotFoundException;
import com.example.app.model.CreateItemRequest;
import com.example.app.model.Item;
import com.example.app.service.ItemService;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.autoconfigure.web.servlet.WebMvcTest;
import org.springframework.boot.test.mock.mockito.MockBean;
import org.springframework.context.annotation.Import;
import org.springframework.http.MediaType;
import org.springframework.test.web.servlet.MockMvc;

import java.time.Instant;
import java.util.List;

import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

@WebMvcTest(ItemController.class)
@Import(GlobalExceptionHandler.class)
class ItemControllerTest {

    @Autowired
    private MockMvc mockMvc;

    @Autowired
    private ObjectMapper objectMapper;

    @MockBean
    private ItemService itemService;

    @Test
    @DisplayName("GET /api/v1/items returns list of items with 200 OK")
    void shouldListItemsSuccessfully() throws Exception {
        Item item = new Item("item-1", "Test Item", "Description", "PENDING", Instant.now());
        when(itemService.getAllItems()).thenReturn(List.of(item));

        mockMvc.perform(get("/api/v1/items")
                        .accept(MediaType.APPLICATION_JSON))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$[0].id").value("item-1"))
                .andExpect(jsonPath("$[0].title").value("Test Item"));
    }

    @Test
    @DisplayName("GET /api/v1/items/{id} returns item when found")
    void shouldReturnItemById() throws Exception {
        Item item = new Item("item-1", "Test Item", "Description", "PENDING", Instant.now());
        when(itemService.getItemById("item-1")).thenReturn(item);

        mockMvc.perform(get("/api/v1/items/item-1")
                        .accept(MediaType.APPLICATION_JSON))
                .andExpect(status().isOk())
                .andExpect(jsonPath("$.id").value("item-1"))
                .andExpect(jsonPath("$.title").value("Test Item"));
    }

    @Test
    @DisplayName("GET /api/v1/items/{id} returns 404 when item not found")
    void shouldReturn404WhenItemNotFound() throws Exception {
        when(itemService.getItemById("item-999"))
                .thenThrow(new ResourceNotFoundException("Item", "item-999"));

        mockMvc.perform(get("/api/v1/items/item-999")
                        .accept(MediaType.APPLICATION_JSON))
                .andExpect(status().isNotFound())
                .andExpect(jsonPath("$.code").value("RESOURCE_NOT_FOUND"))
                .andExpect(jsonPath("$.message").value("Item with id 'item-999' was not found"));
    }

    @Test
    @DisplayName("POST /api/v1/items creates new item and returns 201 Created")
    void shouldCreateItemSuccessfully() throws Exception {
        CreateItemRequest request = new CreateItemRequest("New Item", "Description");
        Item created = new Item("item-2", "New Item", "Description", "PENDING", Instant.now());
        when(itemService.createItem(any(CreateItemRequest.class))).thenReturn(created);

        mockMvc.perform(post("/api/v1/items")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(request)))
                .andExpect(status().isCreated())
                .andExpect(jsonPath("$.id").value("item-2"))
                .andExpect(jsonPath("$.title").value("New Item"));
    }

    @Test
    @DisplayName("POST /api/v1/items returns 400 Bad Request when title is blank")
    void shouldRejectBlankTitleOnCreate() throws Exception {
        CreateItemRequest invalidRequest = new CreateItemRequest("   ", "Valid description");

        mockMvc.perform(post("/api/v1/items")
                        .contentType(MediaType.APPLICATION_JSON)
                        .content(objectMapper.writeValueAsString(invalidRequest)))
                .andExpect(status().isBadRequest())
                .andExpect(jsonPath("$.code").value("VALIDATION_FAILED"))
                .andExpect(jsonPath("$.details").isArray());

        verify(itemService, never()).createItem(any());
    }

    @Test
    @DisplayName("DELETE /api/v1/items/{id} returns 204 No Content on success")
    void shouldDeleteItemSuccessfully() throws Exception {
        doNothing().when(itemService).deleteItem("item-1");

        mockMvc.perform(delete("/api/v1/items/item-1"))
                .andExpect(status().isNoContent());

        verify(itemService, times(1)).deleteItem("item-1");
    }
}
