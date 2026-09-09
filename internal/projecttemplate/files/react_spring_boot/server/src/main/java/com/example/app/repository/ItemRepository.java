package com.example.app.repository;

import com.example.app.model.Item;
import org.springframework.stereotype.Repository;

import java.time.Instant;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;

@Repository
public class ItemRepository {

    private final ConcurrentMap<String, Item> storage = new ConcurrentHashMap<>();

    public ItemRepository() {
        // Seed baseline item for bootstrap verification
        Item baseline = new Item(
                "item-1",
                "Bootstrap Project Starter",
                "Initial fullstack project baseline initialized by Multigent",
                "COMPLETED",
                Instant.now()
        );
        storage.put(baseline.id(), baseline);
    }

    public List<Item> findAll() {
        List<Item> items = new ArrayList<>(storage.values());
        items.sort(Comparator.comparing(Item::createdAt).reversed());
        return items;
    }

    public Optional<Item> findById(String id) {
        return Optional.ofNullable(storage.get(id));
    }

    public Item save(Item item) {
        storage.put(item.id(), item);
        return item;
    }

    public boolean deleteById(String id) {
        return storage.remove(id) != null;
    }

    public boolean existsById(String id) {
        return storage.containsKey(id);
    }

    public void clear() {
        storage.clear();
    }
}
