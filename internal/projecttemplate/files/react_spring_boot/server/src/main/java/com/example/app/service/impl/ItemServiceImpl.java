package com.example.app.service.impl;

import com.example.app.exception.ResourceNotFoundException;
import com.example.app.model.CreateItemRequest;
import com.example.app.model.Item;
import com.example.app.repository.ItemRepository;
import com.example.app.service.ItemService;
import org.springframework.stereotype.Service;

import java.time.Instant;
import java.util.List;
import java.util.UUID;

@Service
public class ItemServiceImpl implements ItemService {

    private final ItemRepository itemRepository;

    public ItemServiceImpl(ItemRepository itemRepository) {
        this.itemRepository = itemRepository;
    }

    @Override
    public List<Item> getAllItems() {
        return itemRepository.findAll();
    }

    @Override
    public Item getItemById(String id) {
        return itemRepository.findById(id)
                .orElseThrow(() -> new ResourceNotFoundException("Item", id));
    }

    @Override
    public Item createItem(CreateItemRequest request) {
        String id = "item-" + UUID.randomUUID().toString().substring(0, 8);
        Item item = new Item(
                id,
                request.title().trim(),
                request.description() != null ? request.description().trim() : "",
                "PENDING",
                Instant.now()
        );
        return itemRepository.save(item);
    }

    @Override
    public void deleteItem(String id) {
        if (!itemRepository.existsById(id)) {
            throw new ResourceNotFoundException("Item", id);
        }
        itemRepository.deleteById(id);
    }
}
