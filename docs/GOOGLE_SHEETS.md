# Выгрузка в Google Таблицы: настройка (один раз, ~10 минут)

Парсер пишет в таблицу от имени **сервисного аккаунта** — это «робот»
Google с собственным email. Ему, как обычному человеку, нужно открыть
доступ к таблице.

## 1. Проект и API

1. Откройте <https://console.cloud.google.com/> и создайте проект
   (выпадающий список проектов вверху → **New project**).
2. **APIs & Services → Library** → найдите **Google Sheets API** → **Enable**.

Google Sheets API бесплатен в пределах квот (сотни запросов в минуту;
одна выгрузка — 4–5 запросов).

## 2. Сервисный аккаунт и ключ

1. **IAM & Admin → Service Accounts → Create service account**.
   Имя любое, например `webparser`. Роли проекту выдавать не нужно.
2. Откройте созданный аккаунт → вкладка **Keys → Add key → Create new key → JSON**.
   Скачается файл с ключом.
3. Положите его в папку проекта как `credentials.json`.

> ⚠️ Это секрет, как пароль. `credentials*.json` уже в `.gitignore` —
> не коммитьте, не отправляйте в чаты. Утёк — удалите ключ в консоли
> (Keys → корзина) и создайте новый.

## 3. Доступ к таблице

1. Откройте в `credentials.json` поле `client_email` —
   что-то вроде `webparser@my-project.iam.gserviceaccount.com`.
2. Откройте нужную Google Таблицу → **Настройки доступа** →
   добавьте этот email с правами **Редактор**.

## 4. Запуск

```bash
webparser -sheet "https://docs.google.com/spreadsheets/d/<ID>/edit" -credentials credentials.json
```

Лист «Книги» будет создан, если его нет, и **полностью перезаписан** при
каждом запуске. Можно указать другой лист: `-sheet-name "Цены"`.

## Частые ошибки

| Ошибка | Причина |
|---|---|
| `403 The caller does not have permission` | Таблица не открыта для `client_email` (шаг 3) |
| `404 Requested entity was not found` | Неверная ссылка на таблицу |
| `Google Sheets API has not been used in project ...` | API не включён (шаг 1.2) |
| `open credentials.json: no such file` | Неверный путь к ключу |
