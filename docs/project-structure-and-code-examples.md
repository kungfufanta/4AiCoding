# Project Structure

## Overview
In this document, we will explore the essential components of a well-structured project. Understanding the project structure helps maintain clean code and promotes best practices.

## Directory Structure
- **src/**: Contains the main application code.
- **docs/**: Documentation files.
- **tests/**: Unit and integration tests.

## Entity Example
Entities are the core business objects of the application. Each entity usually corresponds to a database table.

### Example: User Entity
```java
public class User {
    private Long id;
    private String name;
    private String email;

    // Getters and Setters
}
```

## Repository
The repository layer interacts with the database. It abstracts data access and provides methods to retrieve and store entities.

### Example: UserRepository
```java
public interface UserRepository extends JpaRepository<User, Long> {
    Optional<User> findByEmail(String email);
}
```

## Service
The service layer contains business logic. It calls repositories and coordinates data flow between entities and controllers.

### Example: UserService
```java
@Service
public class UserService {
    @Autowired
    private UserRepository userRepository;

    public User findUserByEmail(String email) {
        return userRepository.findByEmail(email)
            .orElseThrow(() -> new UserNotFoundException(email));
    }
}
```

## Controller
Controllers handle HTTP requests and responses. They map client requests to service layer methods.

### Example: UserController
```java
@RestController
@RequestMapping("/users")
public class UserController {
    @Autowired
    private UserService userService;

    @GetMapping("/email/{email}")
    public ResponseEntity<User> getUserByEmail(@PathVariable String email) {
        User user = userService.findUserByEmail(email);
        return ResponseEntity.ok(user);
    }
}
```

## DTO (Data Transfer Object)
DTOs are simple objects that carry data between processes. They help in reducing database access and improving performance.

### Example: UserDTO
```java
public class UserDTO {
    private String name;
    private String email;

    // Getters and Setters
}
```

## Exception Handling
Effective exception handling is crucial for providing feedback to API users and for maintaining application reliability.

### Example: Global Exception Handler
```java
@ControllerAdvice
public class GlobalExceptionHandler {
    @ExceptionHandler(UserNotFoundException.class)
    public ResponseEntity<String> handleUserNotFound(UserNotFoundException ex) {
        return ResponseEntity.status(HttpStatus.NOT_FOUND).body(ex.getMessage());
    }
}
```

## Conclusion
Following a structured approach to your project design will ensure that the application remains maintainable, scalable, and easy to understand.