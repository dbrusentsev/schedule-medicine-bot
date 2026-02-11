# Schedule Bot

Telegram-бот для напоминаний о приеме лекарств.

## Возможности

- Добавление напоминаний с произвольным названием лекарства
- Выбор времени напоминания (часы: 06-23, минуты: 00, 15, 30, 45)
- Несколько напоминаний для каждого пользователя
- Ежедневные уведомления в указанное время
- Часовой пояс: Екатеринбург (UTC+5)

## Команды бота

| Команда | Описание |
|---------|----------|
| `/start` | Начать работу с ботом |
| `/add` | Добавить новое напоминание |
| `/list` | Показать список напоминаний |
| `/stop` | Отключить напоминания |
| `/stats` | Статистика бота (только для админа) |

## Переменные окружения

| Переменная | Обязательная | Описание |
|------------|--------------|----------|
| `TELEGRAM_BOT_TOKEN` | Да | Токен бота от @BotFather |
| `ADMIN_ID` | Нет | Telegram ID администратора для доступа к `/stats` |

## Запуск

### Локально

```bash
export TELEGRAM_BOT_TOKEN=your_token_here
export ADMIN_ID=123456789
go run .
```

### Docker Compose

1. Создайте файл `.env`:

```
TELEGRAM_BOT_TOKEN=your_token_here
ADMIN_ID=123456789
```

2. Запустите:

```bash
docker-compose up -d
```

### Docker

```bash
docker build -t scheldue-bot .
docker run -d \
  -e TELEGRAM_BOT_TOKEN=your_token_here \
  -e ADMIN_ID=123456789 \
  -e TZ=Asia/Yekaterinburg \
  scheldue-bot
```

## Сборка

```bash
go build -o scheldue-bot .
```
