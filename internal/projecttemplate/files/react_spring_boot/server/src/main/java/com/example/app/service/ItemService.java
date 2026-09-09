package com.example.app.service;

import com.example.app.model.CreateItemRequest;
import com.example.app.model.Item;

import java.util.List;

public interface ItemService {

    List<Item> getAllItems();

    Item getItemById(String id);

    Item createItem(CreateItemRequest request);

    void deleteItem(String id);
}
