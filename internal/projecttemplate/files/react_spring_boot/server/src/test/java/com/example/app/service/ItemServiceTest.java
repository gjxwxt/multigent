package com.example.app.service;

import com.example.app.exception.ResourceNotFoundException;
import com.example.app.model.CreateItemRequest;
import com.example.app.model.Item;
import com.example.app.repository.ItemRepository;
import com.example.app.service.impl.ItemServiceImpl;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.extension.ExtendWith;
import org.mockito.ArgumentCaptor;
import org.mockito.Mock;
import org.mockito.junit.jupiter.MockitoExtension;

import java.time.Instant;
import java.util.List;
import java.util.Optional;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;

@ExtendWith(MockitoExtension.class)
class ItemServiceTest {

    @Mock
    private ItemRepository itemRepository;

    private ItemService itemService;

    @BeforeEach
    void setUp() {
        itemService = new ItemServiceImpl(itemRepository);
    }

    @Test
    @DisplayName("getAllItems returns items sorted from repository")
    void shouldReturnAllItems() {
        Item item = new Item("item-1", "Test", "Desc", "PENDING", Instant.now());
        when(itemRepository.findAll()).thenReturn(List.of(item));

        List<Item> result = itemService.getAllItems();

        assertThat(result).hasSize(1);
        assertThat(result.get(0).id()).isEqualTo("item-1");
        verify(itemRepository, times(1)).findAll();
    }

    @Test
    @DisplayName("getItemById returns item when found")
    void shouldReturnItemByIdWhenFound() {
        Item item = new Item("item-1", "Test", "Desc", "PENDING", Instant.now());
        when(itemRepository.findById("item-1")).thenReturn(Optional.of(item));

        Item result = itemService.getItemById("item-1");

        assertThat(result).isNotNull();
        assertThat(result.title()).isEqualTo("Test");
    }

    @Test
    @DisplayName("getItemById throws ResourceNotFoundException when not found")
    void shouldThrowExceptionWhenItemNotFound() {
        when(itemRepository.findById("non-existent")).thenReturn(Optional.empty());

        assertThatThrownBy(() -> itemService.getItemById("non-existent"))
                .isInstanceOf(ResourceNotFoundException.class)
                .hasMessageContaining("non-existent");
    }

    @Test
    @DisplayName("createItem trims title, assigns UUID and sets status to PENDING")
    void shouldCreateItemWithGeneratedIdAndPendingStatus() {
        CreateItemRequest request = new CreateItemRequest("  New Task  ", "  Detailed notes  ");
        when(itemRepository.save(any(Item.class))).thenAnswer(invocation -> invocation.getArgument(0));

        Item created = itemService.createItem(request);

        assertThat(created.id()).startsWith("item-");
        assertThat(created.title()).isEqualTo("New Task");
        assertThat(created.description()).isEqualTo("Detailed notes");
        assertThat(created.status()).isEqualTo("PENDING");
        assertThat(created.createdAt()).isNotNull();

        ArgumentCaptor<Item> captor = ArgumentCaptor.forClass(Item.class);
        verify(itemRepository).save(captor.capture());
        assertThat(captor.getValue().title()).isEqualTo("New Task");
    }

    @Test
    @DisplayName("deleteItem throws ResourceNotFoundException when item does not exist")
    void shouldThrowWhenDeletingNonExistentItem() {
        when(itemRepository.existsById("missing-id")).thenReturn(false);

        assertThatThrownBy(() -> itemService.deleteItem("missing-id"))
                .isInstanceOf(ResourceNotFoundException.class);

        verify(itemRepository, never()).deleteById(any());
    }

    @Test
    @DisplayName("deleteItem calls repository deleteById when item exists")
    void shouldDeleteWhenItemExists() {
        when(itemRepository.existsById("item-1")).thenReturn(true);

        itemService.deleteItem("item-1");

        verify(itemRepository, times(1)).deleteById("item-1");
    }
}
