from fastapi import FastAPI
from pydantic import BaseModel
from transformers import AutoTokenizer, AutoModelForSequenceClassification, AdamW
import torch
import json
import os
from torch.utils.data import DataLoader, TensorDataset
from torch.cuda.amp import autocast, GradScaler
import psutil

# Функция для мониторинга использования памяти
def monitor_memory():
    memory = psutil.virtual_memory()
    print(f"Использование памяти: {memory.percent}%")

# Путь к модели внутри контейнера (смонтирован из Docker)
model_path = "/app/model_data"
tokenizer = AutoTokenizer.from_pretrained(model_path)
model = AutoModelForSequenceClassification.from_pretrained(model_path, use_safetensors=False)

# Использование GPU, если доступно
device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
model.to(device)

# Инициализация FastAPI
app = FastAPI()

# Списки для хранения ложноположительных и спам-сообщений
false_positives = []
spam_messages = []

# Модель данных для запроса
class TextItem(BaseModel):
    text: str

# Функция для предсказания спама
def predict(text: str) -> str:
    inputs = tokenizer(text, return_tensors="pt", truncation=True, max_length=256).to(device)
    with torch.no_grad():
        outputs = model(**inputs)
        logits = outputs.logits
        predicted_class = torch.argmax(logits, dim=1).item()
    return "Спам" if predicted_class == 1 else "Не спам"

# Маршрут для проверки текста
@app.post("/predict/")
async def classify_text(item: TextItem):
    result = predict(item.text)
    return {"result": result}

# Маршрут для сохранения ложноположительных срабатываний
@app.post("/save_false_positive/")
async def save_false_positive(item: TextItem):
    global false_positives
    false_positives.append(item.text)
    with open('/app/model_data/false_positives.json', 'w') as f:
        json.dump(false_positives, f)
    return {"message": "Ложноположительное сообщение сохранено"}

# Маршрут для сохранения спам-сообщений
@app.post("/save_spam/")
async def save_spam(item: TextItem):
    global spam_messages
    spam_messages.append(item.text)
    with open('/app/model_data/spam_messages.json', 'w') as f:
        json.dump(spam_messages, f)
    return {"message": "Спам-сообщение сохранено"}

# Маршрут для переобучения модели
@app.post("/retrain/")
async def retrain_model():
    global model, tokenizer
    global false_positives, spam_messages
    if not false_positives and not spam_messages:
        return {"message": "Нет данных для переобучения"}

    # Токенизация спам-сообщений и ложноположительных срабатываний
    inputs = tokenizer(false_positives + spam_messages, return_tensors="pt", truncation=True, padding=True, max_length=256).to(device)

    # Метки: 0 для ложноположительных (не спам), 1 для спама
    labels = torch.tensor([0] * len(false_positives) + [1] * len(spam_messages)).to(device)

    # Использование DataLoader для разбиения данных на батчи
    dataset = TensorDataset(inputs['input_ids'], inputs['attention_mask'], labels)
    dataloader = DataLoader(dataset, batch_size=8, shuffle=True)

    # Использование смешанной точности
    scaler = GradScaler()
    accumulation_steps = 4  # Градиентное накопление
    optimizer = AdamW(model.parameters(), lr=1e-5, no_deprecation_warning=True)
    model.train()

    for i, batch in enumerate(dataloader):
        input_ids, attention_mask, labels = batch
        optimizer.zero_grad()  # Обнуление градиентов

        with autocast():  # Смешанная точность
            outputs = model(input_ids=input_ids, attention_mask=attention_mask, labels=labels)
            loss = outputs.loss / accumulation_steps  # Делим на количество шагов

        scaler.scale(loss).backward()

        if (i + 1) % accumulation_steps == 0:
            scaler.step(optimizer)
            scaler.update()

        # Явное освобождение памяти
        del input_ids, attention_mask, labels, outputs
        torch.cuda.empty_cache()  # Очистка кеша CUDA (если используется GPU)

        # Мониторинг использования памяти
        monitor_memory()

    # Сохранение обновлённой модели и токенизатора
    model.save_pretrained(model_path)
    tokenizer.save_pretrained(model_path)

    # Повторная загрузка модели и токенизатора после сохранения
    model = AutoModelForSequenceClassification.from_pretrained(model_path, use_safetensors=False).to(device)
    tokenizer = AutoTokenizer.from_pretrained(model_path)

    # Очистка списков после обучения
    false_positives = []
    spam_messages = []
    with open('/app/model_data/false_positives.json', 'w') as f:
        json.dump(false_positives, f)
    with open('/app/model_data/spam_messages.json', 'w') as f:
        json.dump(spam_messages, f)

    return {"message": "Модель успешно переобучена"}

# Запуск сервера
if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8001)
