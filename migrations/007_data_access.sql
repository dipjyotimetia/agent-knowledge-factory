-- A third knowledge type: where to query a source, without a copy of its rows.
ALTER TABLE knowledge_items DROP CONSTRAINT IF EXISTS knowledge_items_item_type_check;
ALTER TABLE knowledge_items ADD CONSTRAINT knowledge_items_item_type_check
    CHECK (item_type IN ('concept', 'skill', 'data-access'));
